# DTail performance plan (2026-09-15)

This document records the optimization plan derived from profiling the fork
after the upstream comparison in `BENCHMARK_RESULTS.md`, the baseline numbers
to beat, how to benchmark each change, and a results table to fill in as tasks
complete. Task IDs refer to the `ask` task list of this repository.

**Rule for every task:** the full test suite must succeed after every completed
change. Nothing is done until this passes:

```bash
make clean && make build
make test
DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test
make vet && make lint
```

Every change must also keep output byte-identical (see "How to benchmark").

## Baseline (this machine, 100 MiB inputs)

Measured on 2026-09-15 with the binaries at commit `c472f83`. Host: Rocky 9
bhyve guest, Intel N100, 4 vCPUs, Linux 5.14, clocksource `hpet`.

| Scenario | Lines | Elapsed | User | Sys | Notes |
|---|---:|---:|---:|---:|---|
| dcat server mode | 818,561 | 8.15 s | 2.24 s | 6.93 s | client is syscall bound; single run, re-measured as 8.54-10.59 s before `y4` (see Results) |
| dcat serverless | 818,561 | 0.30 s | 0.14 s | 0.05 s | 55% in write syscalls |
| dmap aggregate serverless | 820,044 | 1.64 s | 1.68 s | 0.23 s | 398 MB allocated, 133 GCs |
| dgrep ERROR serverless | 818,561 | 0.20 s | 0.11 s | 0.09 s | |

Client CPU profile of the server-mode dcat run: 73% of samples in `time.Now`,
called once per received line from `internal/io/dlog/dlog.go` `Raw`. The
vDSO is mapped on this host and Go takes its vDSO path, but the clocksource
is `hpet`, which the vDSO cannot read in user space, so the kernel's vDSO
code falls back to a real syscall. Both `clock_gettime` calls Go makes per
`time.Now` (`CLOCK_REALTIME` then `CLOCK_MONOTONIC`, see the Go runtime's
`time_linux_amd64.s`) therefore become real syscalls. strace counted about
1.8M `clock_gettime` calls for one run: 2 x 818,561 = 1.64M from `Raw`; the
remaining ~0.16M were not attributed (runtime `nanotime` reads and timers
are the likely source). One `time.Now` (both calls, user and kernel time
together) costs about 8.3 µs here, measured separately on 2026-09-15. Over
818,561 lines that is about 6.8 s, which roughly accounts for the drop in
client sys time from before to after `y4` (7.09-7.34 s to 0.62-0.71 s, see
Results). It is not the whole sys time: part of the 8.3 µs is user time,
and after the change sys is still 0.62-0.71 s from the unattributed clock
reads and network syscalls.
The production r0 to r2 dserver hosts are bhyve guests as well, so this
matters in production and not only in benchmarks. On hosts whose clocksource
the vDSO can read (for example `tsc`) the same change still removes about
50 ns per line.

Server CPU profile of the same run: 16% in `activityConn.refreshDeadline`
(one `time.Now` plus `SetDeadline` per TCP write), the rest in SSH packet
writes and the read path.

dmap profile: 18% in the regexp backtracker because the dmap line filter
`\|MAPREDUCE:STATS\|` is not recognized as a literal pattern; the remainder
is per-line map and string allocation in the parser and aggregator.

## Plan

Tier 1 targets the server-mode transfer path, tier 2 the dmap CPU path,
tier 3 smaller cleanups. Expected effect column is an estimate from the
profiles, to be replaced by measurements in the results table.

| # | Task | Change | Files | Expected effect |
|---|---|---|---|---|
| 1 | `y4` | Stop calling `time.Now` per payload line in the client logger. Drop the `now` parameter from the `loggers.Logger` sink contract or pass a lazy value; the file sink caches the day string and refreshes it from its existing idle-flush ticker. | `internal/io/dlog/dlog.go`, `internal/io/dlog/loggers/{logger,file,stdout,fout,none}.go` | server-mode dcat 8 s to about 2 s here |
| 2 | `z4` | Rewrite the client receive loop: scan chunks with `bytes.IndexByte`, no per-line `String()` or `+"\n"` allocations, cheap prefix check before `parseAuthKeyMessage`, `[]byte` fast path into the stdout sink using `Write` instead of `fmt.Fprint`. | `internal/clients/handlers/basehandler.go` (`Write`, `handleMessage`, `parseAuthKeyMessage`), `internal/io/dlog/loggers/stdout.go` | remaining client per-line cost roughly halved |
| 3 | `05` | Rate-limit the server idle deadline refresh: atomic activity flag set by Read/Write, per-connection ticker refreshes `SetDeadline` only when activity was seen. | `internal/server/activity_conn.go` | about 16% of server CPU in dcat server mode |
| 4 | `15` | Treat regex patterns whose only metacharacters are backslash-escaped punctuation as literals, so `\|MAPREDUCE:STATS\|` uses `bytes.Contains`. | `internal/regex/regex.go` (`isLiteralPattern`), `internal/clients/query_regex.go` | about 18% of dmap CPU, client and server |
| 5 | `25` | Remove per-line allocations in the aggregator: reuse one fields map per processor, build group keys in a reusable buffer with alloc-free map lookup, avoid the `lineContent.String()` copy, clone only strings retained by `last`/`len` storage. | `internal/mapr/aggregate/aggregate.go`, `internal/mapr/aggregate/groupkey.go`, `internal/mapr/logformat/default.go`, `internal/mapr/aggregateset.go` | large drop in dmap allocations and GC |
| 6 | `35` | Batch lines per `Processor` without a shared mutex per line; take `groupMu` once per batch; check `stopping` once per line. | `internal/mapr/aggregate/{aggregate,batcher}.go` | small dmap CPU win |
| 7 | `45` | `[]byte` fast path from `filteringProcessor.ProcessFilteredRaw` into `DirectLineProcessor` so cat/grep lines skip the pooled buffer round trip. | `internal/io/fs/readfile_processor.go`, `internal/handlers/line_writer.go` | small serverless win |
| 8 | `55` | Hand the 64 KiB batch buffer over instead of copying it twice; make the session generation atomic so `shouldWriteGeneration` takes no mutex per line. | `internal/handlers/line_writer.go`, `internal/handlers/output_manager.go`, `internal/handlers/sessioncommand.go` | small server-mode win |
| 9 | `65` | Evaluate the follow latency floor: 100 ms EOF poll on the server and 100 ms stdout idle flush on the client. Decide with measurements whether to shorten either. | `internal/io/fs/readfile_processor_optimized.go`, `internal/io/dlog/loggers/stdout.go` | follow benchmark only |
| 10 | `75` | Closure: rerun the full benchmark, fill in the results table below, update `BENCHMARK_RESULTS.md`. | this document | |

Not planned: SSH cipher tuning. The server uses the x/crypto defaults,
AES-GCM was under 2% of server CPU, and packet size and window are fixed by
the library.

## How to benchmark

Quick check for a single change, on this machine:

1. Build: `make clean && make build`.
2. Generate data once (the same generators as the benchmark script):
   the `_generate_normal_data` and `_generate_stats_data` awk snippets in
   `benchmarks/upstream_vs_local_bench.sh`, 100 MiB each.
3. Start a server with pprof:
   `cd <dir with cache/> && ./dserver --cfg none --logger stdout --logLevel error --bindAddress 127.0.0.1 --port 2299 --pprof 127.0.0.1:6099`
   with `cache/<user>.authorized_keys` and `cache/ssh_host_key` prepared as
   `_prepare_ssh_material` does.
4. Time the client and record user/sys time, for example with
   `benchmarks/measure_command.py --stdout out.txt --stderr err.txt -- <client command>`.
   Client command shape:
   `DTAIL_SSH_PRIVATE_KEYFILE_PATH=<key> ./dcat --cfg none --plain --noColor --logger stdout --logLevel error --trustAllHosts --servers 127.0.0.1:2299 --files <file>`.
   Add `--cpuprofile --profiledir <dir>` for a client profile and
   `curl -o server.prof "http://127.0.0.1:6099/debug/pprof/profile?seconds=N"`
   for a server profile; inspect with `go tool pprof -top`.
5. Verify correctness: `cmp out.txt <input file>` must report no difference
   for dcat; for dgrep compare against `grep`; for dmap compare the
   canonicalized table against the pre-change output.

Full comparison against upstream and the fork's own previous commit:

```bash
benchmarks/upstream_vs_local_bench.sh smoke                # correctness only
benchmarks/upstream_vs_local_bench.sh run --iterations 5   # timings
```

The script requires a clean tracked tree, an upstream checkout at
`../dtail-upstream`, and sudo for cache dropping. To compare against the
fork's own baseline instead of upstream, check out commit `c472f83` into a
second directory and pass it as `--upstream-root`.

What to watch per tier: tier 1 should push client sys time toward zero;
tier 2 should reduce the `total_alloc` value printed in the client's
shutdown metrics line well below 398 MB and the `num_gc` count accordingly.

## Results

Fill in after each task completes. Use the same data files and machine as the
baseline, and note the commit.

| Task | Commit | Scenario | Before | After | Verified identical output | Tests pass |
|---|---|---|---:|---:|---|---|
| `y4` | parent `c472f83` | dcat server mode, `--logger stdout` (3 runs, elapsed / user / sys) | 8.54-10.59 s / 2.32-2.40 s / 7.09-7.34 s | 1.24-1.42 s / 1.10-1.14 s / 0.62-0.71 s | yes, `cmp` against input every run | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `y4` | parent `c472f83` | dcat server mode, default `fout` logger | 8.90 s / 2.43 s / 7.36 s | 1.42 s / 1.21 s / 0.70 s | yes, `cmp`; daily log file gets no payload | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `y4` | parent `c472f83` | dcat server mode, `fout --log-payload` | not measured | 3.91 s / 3.47 s / 3.61 s | yes, stdout and file tee both identical to input | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `z4` | parent `85475b5` | dcat server mode, `--logger stdout` (5 interleaved runs, elapsed / user / sys) | 1.26-1.31 s / 1.09-1.19 s / 0.58-0.64 s | 0.82-0.97 s / 0.33-0.37 s / 0.43-0.51 s | yes, `cmp` against input every run | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `z4` | parent `85475b5` | dcat server mode, default `fout` logger (5 interleaved runs) | 1.30-1.36 s / 1.09-1.18 s / 0.58-0.67 s | 0.82-0.99 s / 0.36-0.41 s / 0.44-0.49 s | yes, `cmp` against input every run | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `z4` | parent `85475b5` | client profile, dcat server mode, `--logger stdout` | 1.57 s CPU samples, 273 MB total_alloc, 76 GCs | 0.71 s CPU samples, 128 MB total_alloc, 60 GCs | yes | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `z4` | parent `85475b5` | `BenchmarkBaseHandlerWrite`, 32 KiB chunk of 136-byte lines | 201 µs/op, 163 MB/s, 510 allocs/op | 10 µs/op, 3.28 GB/s, 0 allocs/op | n/a (unit tests compare against the legacy loop) | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `05` | parent `59356ae` | dserver CPU (utime+stime from `/proc`) per dcat server-mode run, `--logger stdout` (5 interleaved runs) | 1.25-1.34 s | 1.02-1.07 s (one outlier 1.33 s) | yes, `cmp` against input every run | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `05` | parent `59356ae` | client elapsed / user / sys, same runs | 0.85-0.97 s / 0.32-0.39 s / 0.43-0.49 s | 0.72-0.87 s / 0.31-0.40 s / 0.39-0.50 s | yes | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `05` | parent `59356ae` | server CPU profile, dcat server mode | `activityConn.refreshDeadline` 19% of samples (`time.Now` 8%, `SetDeadline` 7%) | no deadline refresh in the profile | yes | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `15` | parent `b0340f5` | dmap aggregate serverless, 100 MiB stats log (3 interleaved rounds, elapsed / user / sys) | 1.82-1.87 s / 2.07-2.13 s / 0.30-0.36 s | 1.52-1.72 s / 1.82-2.02 s / 0.36-0.39 s | yes, raw `cmp` and canonicalized table every round | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `15` | parent `b0340f5` | dmap count serverless, same log (2 interleaved rounds) | 1.50-1.62 s / 1.78-1.93 s / 0.33-0.40 s | 1.30-1.46 s / 1.62-1.80 s / 0.42-0.43 s | yes, raw `cmp` and canonicalized table every round | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `15` | parent `b0340f5` | client CPU profile, dmap aggregate serverless | `regexp.(*Regexp).backtrack` 14.3% cumulative of 1.75 s samples | no `regexp` samples left in the profile (1.68 s samples) | yes | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `15` | parent `b0340f5` | `BenchmarkMaprFilterPattern`, `\|MAPREDUCE:STATS\|` against 3 lines per op | 702-747 ns/op (compiled regexp) | 403-411 ns/op (literal search) | n/a (unit tests compare the literal against regexp) | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `25` | parent `7491087` | dmap aggregate serverless, 100 MiB stats log (3 interleaved rounds, elapsed / user / sys) | 1.53-1.69 s / 1.81-2.00 s / 0.35-0.38 s | 0.79-0.80 s / 0.78-0.79 s / 0.04-0.05 s | yes, raw `cmp` and canonicalized table every round | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `25` | parent `7491087` | dmap count serverless, same log (2 interleaved rounds) | 1.36-1.38 s / 1.62-1.66 s / 0.37-0.39 s | 0.60-0.62 s / 0.59-0.60 s / 0.04 s | yes, raw `cmp` and canonicalized table every round | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `25` | parent `7491087` | dmap aggregate serverless, client shutdown metrics line (2 runs per binary) | 394.69-394.76 MB total_alloc, 134-135 GCs | 24.24-24.25 MB total_alloc, 8 GCs | yes, `cmp` of the two profiled runs | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `25` | parent `7491087` | `BenchmarkDefaultParserMakeFields`, `all_fields` (allocating form) vs `into_reused_map` (reused map), 200k iterations | 1845 ns/op, 1240 B/op, 4 allocs/op | 759 ns/op, 0 B/op, 0 allocs/op | n/a (unit tests compare `MakeFieldsInto` against `MakeFields` for every built-in parser) | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `35` | parent `d5cae8f` | dmap aggregate serverless, 100 MiB stats log (5 interleaved rounds, elapsed / user / sys) | 0.80-0.85 s / 0.79-0.81 s / 0.03-0.07 s | 0.76-0.78 s / 0.75-0.76 s / 0.03-0.04 s | yes, raw `cmp` and canonicalized table every round | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `35` | parent `d5cae8f` | dmap count serverless, same log (5 interleaved rounds) | 0.62-0.64 s / 0.59-0.62 s / 0.04-0.06 s | 0.57-0.61 s / 0.56-0.57 s / 0.02-0.06 s | yes, raw `cmp` and canonicalized table every round | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `35` | parent `d5cae8f` | dmap aggregate serverless, the same log split into 4 files of 25 MiB (5 interleaved rounds) | 0.79-0.83 s / 1.54-1.60 s / 0.05-0.07 s | 0.42-0.50 s / 0.80-0.92 s / 0.04-0.05 s | yes, raw `cmp` and canonicalized table every round | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `35` | parent `d5cae8f` | `BenchmarkProcessorProcessLine` (new), 1M lines, 6 runs, ns per line | `processors_1` 798-831 ns/op, `processors_4` 868-896 ns/op | `processors_1` 809-859 ns/op, `processors_4` 607-667 ns/op | n/a | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `35` review fixes | parent `8a2b225` | dmap aggregate / count / aggregate over 4 files, serverless, 100 MiB stats log (5 interleaved rounds, elapsed) | aggregate 0.76-0.79 s (plus one cold 1.13 s first run), count 0.57-0.58 s, 4 files 0.42-0.52 s | aggregate 0.76-0.77 s, count 0.57-0.60 s, 4 files 0.42-0.52 s | yes, raw `cmp` and canonicalized table every round | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `35` retention fix | parent `84eb1e1` | allocations per 100-line batch, steady identical lines, `GOMAXPROCS(1)`, `TestProcessorSteadyLargeLinesAllocationFree` (default parser, group by a 3000 / 4000 byte `color`; copying parser, 150 / 300 fields) | keys 22 / 68, fields 352 / 861 (`d5cae8f`: keys 1 / 1) | keys 0 / 0, fields 0 / 0 | n/a | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `35` batch-maximum fix | parent `54568ac` | allocations per 100-line batch, `GOMAXPROCS(1)`, `TestProcessorVaryingKeyLengthsAllocationFree` (default parser, group by `color`, 64 groups with key lengths uniform in 0 to 2 / 4 / 8 / 16 KiB, random line order, fixed seed, 50 warm-up batches, `AllocsPerRun(200)`) | 0 / 0 / 26 / 53 (`d5cae8f`: 1 / 1 / 1 / 1, reviewer measurement) | 0 / 0 / 0 / 0 | n/a | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `35` batch-history fix | parent `df3e571` | allocations per cycle, `GOMAXPROCS(1)`, steady state, each cycle measured on its own: `TestProcessorSmallerBatchesAllocationFree` (default parser, group by `color`: a full batch of 2700 / 3000 byte keys, then one short line drained by `Flush`; a full batch of 3000 byte keys, then a full batch of short keys) and `TestBatchScratchSmallerBatchesAllocationFree` (fields: 100 lines of 300 fields, then 1 / 100 lines of 3 fields) | keys 22 / 22 / 22, fields 852 / 852 (measured with the history set to one batch, which is the `cc15be4` algorithm; the same tests on `cc15be4` itself also fail with 22 and 852) | keys 0 / 0 / 0, fields 0 / 0 | n/a | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `45` | parent `c47e93f` | dcat serverless, 1 GiB normal log (the 100 MiB log 10 times, 8,185,610 lines), `--plain` (5 interleaved rounds, elapsed / user / sys, median in parentheses) | 1.79-4.54 (2.08) s / 1.25-3.64 (1.38) s / 0.57-2.02 (0.71) s | 1.39-1.83 (1.40) s / 0.84-1.08 (0.85) s / 0.53-0.84 (0.55) s | yes, `cmp` against input every run | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `45` | parent `c47e93f` | dgrep `--regex INFO` (90% of lines match) serverless, same 1 GiB log, same runs | 1.89-3.06 (2.58) s / 1.31-1.53 (1.47) s / 0.51-0.74 (0.64) s | 1.53-2.25 (1.68) s / 0.94-1.06 (0.95) s / 0.51-0.68 (0.64) s | yes, `cmp` against `grep INFO` every run | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `45` | parent `c47e93f` | dgrep `--regex ERROR` (10% of lines match) serverless, same 1 GiB log, same runs | 0.94-2.08 (0.94) s / 0.70-1.66 (0.72) s / 0.19-0.76 (0.20) s | 0.87-1.24 (0.91) s / 0.63-0.79 (0.65) s / 0.19-0.32 (0.21) s | yes, `cmp` against `grep ERROR` every run | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `45` | parent `c47e93f` | serverless, 100 MiB normal log (10 interleaved rounds, median user time) | dcat 0.13 s, dgrep INFO 0.14 s, dgrep ERROR 0.07 s | dcat 0.09 s, dgrep INFO 0.10 s, dgrep ERROR 0.07 s | yes, `cmp` every run | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `45` | parent `c47e93f` | server mode, 100 MiB normal log, `--logger stdout` (8 interleaved rounds; client elapsed, dserver CPU from `/proc`) | dcat 0.73-0.93 s, server 1.05-1.17 s; dgrep ERROR 0.22-0.28 s, server 0.21-0.35 s | dcat 0.74-0.95 s, server 1.01-1.13 s; dgrep ERROR 0.21-0.27 s, server 0.23-0.34 s (no measurable change) | yes, `cmp` every run | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `45` | parent `c47e93f` | `BenchmarkDirectLineProcessorLinePath` (new), 128-byte line into a plain serverless `DirectWriter` on `io.Discard`, 6 runs | `buffer` 61.4-61.8 ns/op, 0 allocs/op | `raw` 36.2-36.3 ns/op, 0 allocs/op | n/a (unit test compares both paths byte for byte for six writer formats) | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `55` | parent `f290a31` | dcat server mode, 100 MiB normal log, `--logger stdout` (8 interleaved rounds; client elapsed / user / sys, dserver CPU from `/proc`) | 0.77-0.88 s / 0.33-0.39 s / 0.41-0.45 s, server 0.99-1.22 s | 0.63-0.76 s / 0.30-0.39 s / 0.41-0.49 s, server 0.75-0.87 s | yes, `cmp` against input every run | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `55` | parent `f290a31` | dgrep `--regex ERROR` server mode, same log and runs | 0.21-0.26 s elapsed, server 0.23-0.32 s | 0.19-0.23 s elapsed, server 0.20-0.26 s (ranges overlap) | yes, `cmp` against `grep ERROR` every run | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `55` | parent `f290a31` | dmap aggregate server mode, 100 MiB stats log, same runs | 0.85-0.86 s elapsed, server 0.80-0.82 s | 0.84-0.85 s elapsed, server 0.79-0.82 s (no change) | yes, canonicalized table identical to the before client every round | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `55` | parent `f290a31` | dcat server mode, dserver with only the atomic generation change vs `f290a31` (5 interleaved rounds after one cold round) | server 1.00-1.09 s | server 1.00-1.09 s (no measurable change) | yes, `cmp` every run | n/a (measurement build only) |
| `55` | parent `f290a31` | `BenchmarkNetworkWriterToOutputManager` (new), 128-byte lines through writer, queue and reader, 6 runs | 258-284 ns/op, 256 B/op | 153-159 ns/op, 144 B/op | n/a (unit tests compare the bytes read with the bytes written) | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `55` fix | parent `7a16500` | retained heap after GC, 1000 `NetworkWriter`s each doing 3 one-line write+`Flush` cycles (idle follow readers) | `7a16500`: 70.6 MiB (each writer keeps a 72 KiB batch reservation); `f290a31`: 0.3 MiB | 0.3 MiB | n/a (unit test asserts writer buffer capacity) | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `55` fix | parent `7a16500` | dcat server mode, 100 MiB normal log, `--logger stdout`, three dservers side by side (`f290a31`, `7a16500`, fix), 9 rounds rotating the order; dserver CPU from `/proc`, min / median / max | `f290a31` 0.92 / 1.06 / 1.64 s; `7a16500` 0.74 / 0.78 / 0.90 s | 0.73 / 0.77 / 0.89 s (hand-over gain kept) | yes, `cmp` against input every run | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `55` fix 2 | parent `5a9ac06` | reviewer benchmark, follow catch-up in protocol format (655 99-byte lines per chunk, `Flush` per chunk), 3 runs of 2000 iterations | `5a9ac06` 360-375 us/op, 333 KB/op, retained/payload 1.335; `7a16500` 213-253 us/op, 142 KB/op, 1.083; `f290a31` 269-285 us/op, 216 KB/op, 1.000 | 218-246 us/op, 142 KB/op, 1.083 | n/a (unit tests compare batch bytes) | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `55` fix 2 | parent `5a9ac06` | reviewer benchmark, plain 70 KiB chunks of 99-byte lines, largest charged queue entry | `5a9ac06` 310-331 us/op, 348 KB/op, 131,072 B; `7a16500` 105-117 us/op, 87 KB/op, 73,728 B; `f290a31` 170-193 us/op, 161 KB/op, 65,600 B | 107-130 us/op, 87 KB/op, 73,728 B | n/a | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `55` fix 2 | parent `5a9ac06` | retained heap after GC, 1000 writers that caught up (three 70 KB chunks) then idle | `5a9ac06` 0.3 MiB; `7a16500` 70.6 MiB; `f290a31` 125.3 MiB | 70.6 MiB until 2 more small flushes after the remainder, then 0.4 MiB (idle-only writers 0.4 MiB) | n/a (unit test asserts writer buffer capacity) | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `55` fix 2 | parent `5a9ac06` | dcat server mode, 100 MiB normal log, four dservers side by side, 16 rounds rotating the order; dserver CPU median / range | `f290a31` 1.06 / 0.92-1.10 s; `7a16500` 0.77 / 0.73-0.88 s; `5a9ac06` 0.78 / 0.71-1.71 s | 0.79 / 0.72-0.88 s (bulk gain kept at `d38aea8`; `0acf3c3` lost about a quarter of it under backpressure, restored by fix 4, see below) | yes, `cmp` against input every run | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `55` fix 3 | parent `d38aea8` | payload queued before backpressure at the default 2 MiB cap, writer into queue, 9 / 12 / 20 / 40 / 100 KiB lines (byte figures in plain format) | `d38aea8` 1,032,304 / 1,032,276 / 1,146,936 / 1,146,908 / 1,433,614 B (charged/payload, protocol format, 1.995 / 1.996 / 1.798 / 1.799 / 1.430); `f290a31` 2,064,608 / 2,064,552 / 2,048,100 / 2,048,050 / 2,048,020 B | 1,843,400 / 1,843,350 / 1,884,252 / 1,884,206 / 1,945,619 B (charged/payload, protocol format, 1.108 / 1.109 / 1.099 / 1.099 / 1.040) | n/a (unit tests compare the bytes read with the bytes written) | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `55` fix 3 | parent `d38aea8` | batches of 99-byte lines queued at the minimum accepted cap (1 KiB `MaxLineLength` + 128 KiB = 132,096 B), plain format | `d38aea8` 1 (73,728 B charged); `f290a31` 2 (131,200 B) | 2 (131,200 B, both copied) | n/a | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `55` fix 3 | parent `d38aea8` | reviewer benchmark (3 runs of 2000 iterations): follow catch-up protocol 64 KiB chunks; bulk 1 MiB 99-byte lines; constant 40 KiB flushes | `d38aea8` 201-252 us, 142 KB/op; 1.47-1.49 ms; 79-90 us, 74 KB/op, retained/payload 1.798 | 187-222 us, 142 KB/op; 1.46-1.53 ms; 100-106 us, 99 KB/op, retained/payload 1.000 (`f290a31` 92-109 us, 99 KB/op) | n/a | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `55` fix 4 | parent `0acf3c3` | batches admitted under backpressure, 127-byte lines through a production `NetworkWriter` into a 2 MiB-cap queue, reader reads 32 KiB only while the writer is blocked (deterministic, `TestOutputAdoptsBatchesUnderBackpressure`) | `0acf3c3` rule 0 of 215 (protocol) / 0 of 163 (plain) adopted | 219 of 219 / 167 of 167 adopted | yes, the test compares the bytes read with the bytes written | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `55` fix 4 | parent `0acf3c3` | dcat server mode, 100 MiB normal log, instrumented dserver logging every admission attempt of a large payload (3 runs each) | `0acf3c3` rule: 738-899 of 1611 batches adopted, 712-873 copied | 1611 of 1611 adopted, 0 copied | yes, `cmp` against input every run | n/a (measurement build only) |
| `55` fix 4 | parent `0acf3c3` | dcat server mode, 100 MiB normal log, `--logger stdout`, three dservers side by side, 32 rounds (two runs of 16) rotating the order; dserver CPU from `/proc` in ticks (1/100 s), min / median / max | `0acf3c3` 73 / 83 / 100; `d38aea8` 72 / 77 / 102 | 72 / 79.5 / 96 | yes, `cmp` against input every run | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `65` | parent `b3a84a9` | dtail follow, 8 sessions on one dserver, single-line latency on session 0 (40 probes per run, 2 interleaved rounds per variant pair; S100/C100 ranges span the 4 runs of both pairings; mean / p95), server EOF poll S and client stdout flush C | S100/C100 (current): 97-107 ms / 160-172 ms | S50/C50: 49-52 ms / 80-89 ms; S20/C20: 17-20 ms / 29-37 ms; S20/C100: 61-66 ms; S100/C20: 57-62 ms; S50/C100: 65-72 ms; S100/C50: 72-79 ms | n/a (intervals only) | `make clean && make build` only (docs-only change, no code kept) |
| `65` | parent `b3a84a9` | dserver CPU with 8 idle follow sessions (20 s, utime+stime from `/proc`, % of one core), same runs | S100: 6.2-9.2% | S50: 12.1-14.6%; S20: 20.3-27.6% | n/a | `make clean && make build` only (docs-only change, no code kept) |
| `65` | parent `b3a84a9` | dtail client CPU per idle follow session, same runs | C100: 0.89-0.97% | C50: 1.77-2.03%; C20: 4.33-4.45% | n/a | `make clean && make build` only (docs-only change, no code kept) |
| `65` | parent `b3a84a9` | dserver CPU with 1 idle follow session (2 rounds) | S100: 2.8-4.0% | S20: 11.2-11.3% | n/a | `make clean && make build` only (docs-only change, no code kept) |
| `75` | before `c472f83` (pre-plan), after `df52a1e` | serverless dcat 1 GiB, `upstream_vs_local_bench.sh run --iterations 5`, cold cache, elapsed | 1.860-1.902 s (median 1.873) | 1.441-1.502 s (median 1.469) | yes, the script's smoke correctness checks passed before timing | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `75` | before `c472f83` (pre-plan), after `df52a1e` | serverless dcat 100 MiB, `upstream_vs_local_bench.sh run --iterations 5`, cold cache, elapsed | 0.230-0.397 s (median 0.237) | 0.179-0.273 s (median 0.181), ranges overlap | yes, the script's smoke correctness checks passed before timing | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `75` | before `c472f83` (pre-plan), after `df52a1e` | serverless dgrep high-match 100 MiB, `upstream_vs_local_bench.sh run --iterations 5`, cold cache, elapsed | 0.162-0.235 s (median 0.167) | 0.152-0.186 s (median 0.158), ranges overlap | yes, the script's smoke correctness checks passed before timing | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `75` | before `c472f83` (pre-plan), after `df52a1e` | serverless dgrep low-match 100 MiB, `upstream_vs_local_bench.sh run --iterations 5`, cold cache, elapsed | 0.193-0.237 s (median 0.195) | 0.201-0.251 s (median 0.210), ranges overlap: no measurable change | yes, the script's smoke correctness checks passed before timing | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `75` | before `c472f83` (pre-plan), after `df52a1e` | serverless dmap aggregate 100 MiB, `upstream_vs_local_bench.sh run --iterations 5`, cold cache, elapsed | 1.827-1.887 s (median 1.867) | 0.840-0.901 s (median 0.849) | yes, the script's smoke correctness checks passed before timing | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `75` | before `c472f83` (pre-plan), after `df52a1e` | server dcat 100 MiB, `upstream_vs_local_bench.sh run --iterations 5`, cold cache, elapsed | 8.600-8.687 s (median 8.667) | 0.586-0.699 s (median 0.639) | yes, the script's smoke correctness checks passed before timing | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `75` | before `c472f83` (pre-plan), after `df52a1e` | server dgrep high-match 100 MiB, `upstream_vs_local_bench.sh run --iterations 5`, cold cache, elapsed | 0.951-1.006 s (median 0.991) | 0.318-0.381 s (median 0.350) | yes, the script's smoke correctness checks passed before timing | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `75` | before `c472f83` (pre-plan), after `df52a1e` | server dgrep low-match 100 MiB, `upstream_vs_local_bench.sh run --iterations 5`, cold cache, elapsed | 0.274-0.327 s (median 0.301) | 0.267-0.332 s (median 0.308), ranges overlap: no measurable change | yes, the script's smoke correctness checks passed before timing | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `75` | before `c472f83` (pre-plan), after `df52a1e` | server dmap aggregate 100 MiB, `upstream_vs_local_bench.sh run --iterations 5`, cold cache, elapsed | 1.956-2.053 s (median 1.995) | 0.936-0.958 s (median 0.945) | yes, the script's smoke correctness checks passed before timing | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `75` | before `c472f83` (pre-plan), after `df52a1e` | server dmap count 100 MiB, `upstream_vs_local_bench.sh run --iterations 5`, cold cache, elapsed | 1.664-1.815 s (median 1.759) | 0.751-0.802 s (median 0.760) | yes, the script's smoke correctness checks passed before timing | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |
| `75` | before `c472f83` (pre-plan), after `df52a1e` | server dtail follow 10 MiB burst, `upstream_vs_local_bench.sh run --iterations 5`, cold cache, elapsed | 1.248-1.290 s (median 1.273) | 0.251-0.292 s (median 0.278) | yes, the script's smoke correctness checks passed before timing | yes: make clean && make build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet && make lint |

`y4` notes: both binaries ran against the same `c472f83` dserver on port 2299
in one session; a final baseline rerun (9.01 s / 2.42 s / 7.74 s, not part
of the 3-run range) confirmed no machine drift.
The remaining `--log-payload` cost is the file sink's per-line channel send and
allocation, which this task did not change.

`z4` notes: the before binaries were built from `85475b5` and both clients
ran against that commit's dserver on port 2299, alternating before and after
in each of the 5 rounds. Three earlier after-runs made right after `make
test` measured 1.42-1.46 s / 0.66-0.71 s / 0.93-0.97 s; the interleaved
rounds did not reproduce that, so it is treated as disturbance from the
finishing test run. The client-side `baseHandler.Write` share of client CPU
fell from 46% to about 15% (cumulative); what remains is
mostly network read syscalls and runtime clock and scheduler work. dgrep
(`--regex ERROR`), non-plain dcat and dmap output were also compared against
the before client and are identical.

`05` notes: before and after dservers were built with `go build` from
`59356ae` and from the change, ran side by side on ports 2299 and 2300, and
each round ran the before client against the before server and then the
after client against the after server. Server CPU was read from
`/proc/<pid>/stat` around each client run. The round-2 after run (1.33 s
server CPU, 0.87 s elapsed) was not reproduced in the other rounds or in a
sixth profiled round (1.02 s); it is treated as noise. Read and Write now only
set an atomic flag; a per-connection ticker (timeout/4, clamped to 1-30 s)
extends the deadline when activity was seen, so no idle gap up to the
timeout closes an active session. The original commit (`95234cc`) used
now + timeout + interval and described the idle close as "between the
timeout and the timeout plus two intervals"; the real window was timeout +
interval to timeout + 2 intervals, and that extension left no scheduling
margin at any timeout: a tick handled one interval late landed exactly on the
deadline whenever the timeout was a multiple of the interval (900 s, 61 s,
5 s, 2 s) and past it otherwise (901 s missed by 29 s), so timeout mod
interval only decided how badly a late tick missed, never whether it missed.
The review follow-up extends to now + timeout + 2 intervals, which tolerates a
tick handled less than one interval late for any timeout (a tick exactly one
interval late would refresh on the deadline, racing the poller); an idle session
now closes between timeout + 2 intervals and timeout + 3 intervals after its
last activity (about 960-990 s for the default 900 s timeout). The 1 s floor on the
interval keeps the refresher from waking sub-second, at the price of a
disproportionate window for timeouts under 4 s (they close 2-3 s late); that
trade-off is documented at config.DefaultIdleSessionTimeoutS and pinned by a
test. The remaining `activityConn.Write` cumulative share (28%)
is the underlying TCP write syscall.

`15` notes: the before binary was built from `b0340f5`; before and after ran
alternately in each round against the same 100 MiB `_generate_stats_data` file,
serverless (`--cfg none --plain --noColor --logger stdout --logLevel error`).
A pattern is now treated as a literal when its only metacharacters are
backslash-escaped ASCII punctuation, so the dmap line filter
`\|MAPREDUCE:STATS\|` unescapes to `|MAPREDUCE:STATS|` and is matched with
`bytes.Contains`. Escapes with a meaning of their own (`\d \w \s \D \W \S
\b \B \A \z \Q \E \n \t \a \f \r \v \x41 \x{263a} \p{L}`, octal
escapes, and every other alphanumeric escape), unescaped metacharacters, an
escaped space, a trailing backslash, and patterns holding U+FFFD or invalid
UTF-8 all keep the compiled regexp, which stays compiled in either case as the
fallback.

For a pattern holding a validly encoded U+FFFD (the bytes `ef bf bd`) this is a
change of behaviour, not only of speed: the old `isLiteralPattern` rejected just
`.+*?^$[]{}()|\`, so such a pattern took the `bytes.Contains` path and
`dgrep --grep $'abc\xef\xbf\xbd'` returned only the line holding those exact
bytes. It now takes the compiled regexp, which maps every decoding error in the
input to U+FFFD as well, so the same pattern also returns lines holding
`abc\xff` and `abc\xfe`: a superset of what it matched before. Checked end to
end against a four line file with `dgrep` built from `b0340f5` and from this
tree. The regexp semantics are the reference and the intended behaviour, but a
mixed-version fleet can return different lines for a U+FFFD-bearing pattern
until every host is upgraded.

A pattern holding invalid UTF-8 is not part of that change. `regexp.Compile`
rejects such a pattern outright (`error parsing regexp: invalid UTF-8`), and the
old `newRegex` compiled the pattern on its literal path as well and returned the
same error, so `regex.New("abc\xff", Default)` failed before this change just
as it does after it. `dgrep --grep $'abc\xff'` exits 1 with that compile error
in both versions, because `internal/clients/baseclient.go` makes it fatal;
checked with both binaries. There is no behaviour change and no mixed-version
hazard for invalid-UTF-8 patterns. An earlier revision of this note, and the
message of commit `a8e5e0c`, used `$'abc\xff'` as the example of the widened
match; that was wrong, only U+FFFD-bearing patterns widen.

Correctness is pinned by a table test, by a check of every ASCII escape against
`regexp` itself, by a seeded randomized differential test, and by
`FuzzLiteralPattern`, whose body skips patterns over 1 KiB and inputs over
4 KiB. The bound exists because the reference side of the property
(`regexp.MatchString` and `regexp.Match` on an accepted literal) costs
O(len(pattern) * len(input)) in the worst case, and reaching that worst case
needs an input which keeps re-entering a partial match. The cost is therefore a
property of the pair, not of the size alone. A 64 KiB pattern (`\|` repeated
32768 times) against 64 KiB of `|`, where every position starts a partial match,
spends about 27.5 s matching and starves the fuzzer for the rest of the run once
it is in the corpus. The same pattern against 64 KiB of `a` costs about 12 ms
for one execution of the fuzz body, which recompiles the pattern every time, and
that figure is almost entirely `regexp.Compile` (8-15 ms on its own here) rather
than matching: the two matches on the already compiled regexp take a few tens of
microseconds. At the bound (1 KiB pattern, 4 KiB input) even the adversarial
pair costs single-digit milliseconds.

Fuzz runs use `go test -run XXXnone -fuzz FuzzLiteralPattern -fuzztime 60s
./internal/regex/` and have never failed. Absolute execution totals are not
comparable across runs, because the shared corpus grows between them (211
entries when these notes were first written, 239 now); only runs against the
same corpus state are. Against identical copies of the 239-entry corpus, bounded
runs and an unbounded control all landed between roughly 0.7M and 1.2M
executions per 60 s window, which is the run-to-run spread of the bounded body
by itself, so the bound buys no measurable throughput here; it only guards
against the pathological pair above. A run can also sit at 0/sec for a long
stretch. That is typically the engine minimizing a newly interesting input, a
phase which does not advance the execution counter and which reproduces readily
on a cold corpus, where such inputs are still being found; but a warm run can
stall for about 10 s and still add no input at all, so a stall on its own does
not mean one was found. Per-run execution totals and a ~50 s figure for the
64 KiB pair recorded in earlier revisions of this note did not reproduce and are
withdrawn.

The `literal` hint in the serialized form is now only emitted for patterns which
are their own literal: an older peer trusts that hint verbatim and would search
for the backslashes, while peers of this version derive the literal from the
pattern themselves, so server-mode dmap gets the same optimization without the
hint.

`25` notes: the before binary was built from `7491087` and both binaries ran
alternately in each round against the same 100 MiB `_generate_stats_data` file,
serverless (`--cfg none --plain --noColor --logger stdout --logLevel error`).
The per-line path of the aggregator no longer allocates: the log format parsers
fill a caller-owned map through the new optional `logformat.FieldsIntoParser`
interface instead of allocating one map per line, the line is borrowed from the
pooled `bytes.Buffer` as an `unsafe.String` view instead of being copied by
`lineContent.String()`, and the group key is built in a reusable `[]byte` and
looked up with the allocation-free `m[string(b)]` form. Because everything is
borrowed, every value that outlives its line is copied at the point where it is
retained: a group copies its key when it is inserted, `mapr.AggregateSet`
clones the strings that `last()` and `len()` keep, and `csvParser` clones its
header row. That last one was a live hazard rather than a hypothetical one:
`parseHeaderLine` stored sub-slices of the line for the whole session, which was
only safe because the old code handed the parser a fresh copy of every line. The
per-batch scratch comes from a `sync.Pool` rather than living on the
`Aggregate`, because several file processors run `processRawBatch` concurrently.
`TestAggregateDoesNotRetainRecycledLineBuffers` feeds lines from pooled buffers,
overwrites those buffers in place once the batch has been processed, and fails
on any retained view; `TestBorrowedLineAliasesTheBuffer` is its negative
control, failing if `borrowedLine` ever started copying and thereby made the
retention test vacuous. `last()` and `len()` queries now pay one
`strings.Clone` per matching line, which is the price of not retaining a view
into a recycled buffer. dcat and dgrep do not use this code path; both were run
before and after anyway, and their output still matched the input file and
`grep ERROR` respectively. Not measured for this task: server-mode dmap (only
serverless runs were timed) and the 1 GiB inputs.

`35` notes: the before binary was built from `d5cae8f`; before and after ran
alternately in each round, serverless, with the flags of the `25` runs. The
4-file scenario is the 100 MiB stats log cut with `split -n l/4` and passed as
one comma-separated `--files` list. Each `Processor` now batches its own
file's lines in a slice that only its reader goroutine touches, so the shared
batcher and its mutex per line are gone, and `stopping` is checked once per
line instead of twice. A full batch is aggregated in two phases: every line is
parsed into its own pooled scratch without a lock, then the serializer's group
lock is taken once for the whole batch. Before, the group lock was taken per
line, and with 4 processors 72% of the benchmark's CPU samples were
`runtime.procyield` spinning on it. After, the merge (mostly
`AggregateSet.Aggregate` parsing floats) is still serialized and 55% of the
`processors_4` samples still spin, so the scaling over files is far from
linear; moving value parsing out of the lock, or pre-aggregating per batch,
would be the next step. With a single file the change is within noise in the
microbenchmark; the end-to-end single-file runs came out about 5% faster.
`Flush` now drains the processor's partial batch on every call, which follow
readers do after each read (and the journal reader after each line), so
follow output is not held back. A one-shot read keeps up to 99 lines per file
out of periodic interim results until the batch fills or the file ends; the
final result is unchanged. Lines accepted before a graceful shutdown are still
aggregated when the processor closes, before the final serialization; after an
abort they are discarded. Not measured for this task: server-mode dmap (only
serverless runs were timed; the integration tests cover server mode for
correctness) and the 1 GiB inputs.

`35` review-fix notes (the retention-budget part of this paragraph describes
the `84eb1e1` algorithm and is superseded by the retention-fix, batch-maximum
and batch-history notes below; the other fixes still apply): the follow-up
commit keeps the design and fixes its edges. A panic in the parser, where or set clause part way through a batch now
reaches the caller unchanged (the batch scratch counts the line scratches it
handed out instead of clearing by batch length). Fields of a parser without
`MakeFieldsInto` are copied into the line scratch's own map, because the merge
runs only after the whole batch was parsed and `Parser.MakeFields` may reuse
its map. Only parsers registered from outside take that path: the built-in
`default`, `generic`, `generickv` and `csv` parsers implement `MakeFieldsInto`,
and the other built-ins are not-implemented stubs. A pooled batch scratch's
retained storage is now also capped as a whole, not only per line: at most
8192 fields' worth of map buckets and 256 KiB of group-key buffer beyond the
default size of each line scratch (24 fields, 128 bytes). Default-sized
storage is not charged and never replaced; over budget, the largest oversized
scratches are shrunk back to default size first, so after an outlier batch
ordinary batches again reuse every scratch without allocating (a first version
charged default-sized storage too and admitted scratches in order, which after
one outlier batch made every later ordinary batch allocate, 96 times for four
~60 KiB group keys), and `linesProcessed` no longer counts lines an abort
discards. The end-to-end runs in the row above show no measurable change against `8a2b225`
(output raw-identical every round). `BenchmarkProcessorProcessLine` stays at 0
allocs/op; in one non-interleaved run of 6 each it measured `processors_1`
811-1268 ns/op before (noisy) and 761-784 ns/op after, `processors_4`
602-1277 ns/op before (noisy) and 532-544 ns/op after, so no regression but no
claim of a gain either.

`35` retention-fix notes (the per-scratch reference described here is
superseded by the batch-maximum and batch-history notes below): the batch
budget above charged every byte beyond
the default size, including storage the batch just processed had used. On a
steady workload where every line is moderately large (a group key above about
2.6 KiB or more than about 106 fields, times 100 lines per batch), every batch
therefore shrank the largest scratches and the next batch grew them back: 22
and 68 allocations per 100 lines for 3000 and 4000 byte keys, 352 and 861 for
150 and 300 fields through a copying parser, where `d5cae8f` made 1 (keys;
measured the same way in a worktree). Each line scratch now records what its
line in the batch just processed used (group-key length and field count, zero
if the batch did not use it), and the budget charges only idle headroom:
capacity beyond both that and the default size. Storage the last batch used
is kept whatever its size; it is bounded by the per-line limits (at most 100
times 64 KiB of keys and 100 maps of 1024 fields right after a batch of
nothing but maximal lines). Over budget, the scratches with the largest idle
headroom are shrunk to what their last line used, not to the default size,
so a line of the same size fits again without growing. After an outlier batch
the next ordinary batch trims the outlier storage and the batch after it
allocates nothing, also when the ordinary lines are larger than the default
(in `TestBatchScratchRecoversFromOutlierBatch`, the `84eb1e1` algorithm made
136 allocations in that batch for 8 KiB ordinary keys and 8 for 100 ordinary
fields; shrinking idle headroom to the default size instead of to the last
use still made 16 and 44). Measuring idle headroom against each scratch's own
last line still thrashed on workloads whose line sizes vary from line to line,
see the follow-up below.

`35` batch-maximum follow-up (measuring against the current batch only is
superseded by the batch-history notes below): which line lands in which scratch is arbitrary,
so with group-key lengths that vary from line to line, each scratch settled at
the longest key it had seen while its own last line was a random draw; about
half of every scratch counted as idle, and once keys reached about 5 KiB the
budget was exceeded on every batch, a third of the scratches were shrunk and
the next batch grew them back. The budget now measures idle headroom against
the batch instead: `batchScratch.clear` records the longest group key and the
largest field count of any line of the batch just processed, each scratch is
charged only for capacity beyond the larger of that and the default size, and
a scratch over budget is shrunk to that size. A varying workload keeps the
batch maximum near its own maximum, so its batches reuse every scratch (row
above; `TestBatchScratchVaryingLineSizesAllocationFree` also covers random
field counts up to 300 and 1000, which the per-line algorithm answered with 79
and 213 allocations per batch). With this version an outlier batch was
followed by an ordinary batch with a small maximum, which trimmed the outlier
storage, and the batch after it allocated nothing. The tradeoffs of this
version, measured with 60 KiB group keys among 64-byte ones: a workload
that puts at least one line of size L into every batch lets every scratch keep
up to L (6.4 MB of key buffers per pooled batch scratch for one 60 KiB key per
batch at a random position, 0 allocations), within the per-line bound of 100
times 64 KiB that a batch of maximal lines already reached; and batches that
alternate between many outliers and none still shrink and regrow (21
allocations per batch for 20 outliers every other batch).

`35` batch-history follow-up: measuring idle headroom against the current
batch alone made any single batch whose largest line was smaller than the
previous batches' look like an outlier recovery. A follow-mode `Flush` drains
a partial batch, often a single short line, and a workload may interleave
batches of short and long lines; each such batch shrank the long lines'
scratches and the next full batch grew them back (22 allocations per cycle for
3000 byte keys and 852 for 300 fields, row above; `d5cae8f` made about one per
batch in the reviewer's measurement). The watermark is now the largest group
key and field count of any line of the last 8 batches processed with the
batch scratch (`retentionHistory`, two fixed arrays of 8 ints per pooled batch
scratch updated in `batchScratch.clear`, no allocation), and only headroom
beyond the larger of that and the default size is charged or shrunk. Those
workloads now reuse every scratch (row above). Outlier recovery takes 8
ordinary batches instead of one: measured on a batch scratch, after 20 group
keys of 60 KiB the next 7 ordinary batches make 0 allocations, the 8th trims
the idle storage to the budget (16 allocations) and later ones make 0; after
3000 byte keys drop permanently to 100 bytes the 8th batch makes 11
allocations, and after 300 fields drop to 5 it makes 284, then 0. So a
workload whose line sizes shift permanently downward keeps its former storage
for up to 8 batches; the memory bound is unchanged (idle headroom beyond the
watermark within the batch budget, storage up to the watermark within the
per-line limits). Outliers that recur within 8 batches now keep their storage
(20 outliers of 60 KiB every other batch: 0 allocations per batch; with the
history set to one batch, the `cc15be4` algorithm, 16 per batch and 21 in
some batches), while outliers rarer than that are still trimmed and regrown (20
every 9th batch: 16 allocations to trim and 16 to regrow per 9 batches).

`35` known tradeoff, per-line limits (review finding, not changed): the
per-line retention limits of `clearLineScratch` (64 KiB group key, 1024
fields) now apply to each of the 100 line scratches of a batch scratch, so a
steady workload whose lines exceed them releases and regrows every scratch in
every batch. Measured in the review-fix round after `4d7ed2e` with a throwaway
test (processor, `GOMAXPROCS(1)`, 100 identical lines per batch, 20 warm-up
batches, `AllocsPerRun(50)`): default parser, group by a 70,000 byte `color`,
204 allocations per batch, against 3 at `d5cae8f` measured in a worktree with
the same test (about two per line scratch now: the reset to the default size
and the regrow); 60 KiB and 64 KiB keys make 0 (`d5cae8f`: 1). With the
copying test parser (not a `FieldsIntoParser`), 1101 fields per line make 1912
allocations per batch and 1001 fields make 0 (`AllocsPerRun(20)`; not measured
on `d5cae8f`). Keys above 64 KiB or more than 1024 fields per line are far
outside ordinary logs, and raising the limits would let each pooled batch
scratch park 100 times as much storage, so the algorithm is unchanged and the
tradeoff is documented at `clearLineScratch`.

`45` notes: before binaries were built with `go build` from `c47e93f`; the
after binaries of the 1 GiB and server-mode rows by `make build` from the
change, those of the 100 MiB serverless row by `go build` from the same
source; before and after alternated in each
round, serverless with `--cfg none --plain --noColor --logger stdout --logLevel
error` and output written to a file. The file reader now hands a matching line
of the no-local-context path straight to the processor when the processor
implements the new optional `line.RawProcessor` interface
(`ProcessRawLine([]byte, lineNum, sourceID)`); `DirectLineProcessor` does, so
cat and grep lines no longer go through a pooled `bytes.Buffer` (Get, copy,
format, recycle). The slice is borrowed, not transferred: it is the scanner
token (snapshot reads) or the follow reader's reused partial-line buffer, it is
valid only for the call, and the `LineWriter` formatters copy it into the
writer's own buffer, so there is nothing to recycle and no double recycle is
possible. The fast path is wired up once in `newFilteringProcessor`, only when
no before/after/max context is set, and is used only from
`ProcessFilteredRaw`, whose precondition is the same. Every local-context
line and the aggregate `Processor`, which keeps lines until its batch is
aggregated, keep the owned-buffer `ProcessLine` path; a test pins that the
aggregate `Processor` does not implement the interface. The before binary had
slow outlier runs (dcat up to 4.54 s) that the after binary did not show in
the recorded 1 GiB rounds (an earlier, unrecorded set of 5 rounds with the
same source showed slow outliers for both binaries); the medians and minimums above are the comparison to rely on.
The microbenchmark saves about 25 ns per line, the 1 GiB dcat runs about 65 ns
of user time per line (median 1.38 s to 0.85 s over 8.2M lines); the
difference between the two was not investigated. At a 10% hit rate (dgrep
ERROR) the gain is small because only matching lines ever took the buffer.
Server mode shows no measurable change in client time or dserver CPU: the
ranges overlap in every scenario. dcat and dgrep colored (non-`--plain`)
serverless output of the before and after binaries was also compared on the
first 20,000 lines and is identical. Not measured: follow mode (covered by
`TestTailUsesRawProcessorFastPath` for correctness only).

`55` notes: before binaries were built with `go build` from `f290a31`, the
after binaries by `make build` from the change; a before and an after dserver
(`--cfg none --logger stdout`) ran side by side, and each round ran every
scenario with both, alternating which went first. dserver CPU is utime+stime
from `/proc/<pid>/stat` around each client run. The server-side copies are
gone: a full `NetworkWriter` batch (64 KiB threshold) used to be copied once
into a fresh slice and a second time into the output queue, where same-generation
batches were coalesced into one entry that was regrown geometrically (up to
the 2 MiB cap) by copying. Now the writer hands its batch buffer over and
reserves a new one (72 KiB, so a line of up to 8 KiB that crosses the
threshold fits without regrowing), and the queue adopts a large payload as
its own entry without copying (as first committed, any payload of at least
32 KiB; see the third review fix below for the final rule). Retained-bytes
accounting charges the adopted
slice's full capacity against `OutputBufferMaxBytes`; when that capacity does
not fit but the length does, the payload is copied at exact size as before,
so an admissible payload is never stranded by its spare capacity. Payloads
under 32 KiB (partial follow-mode `Flush` batches) are still copied by the
writer and still coalesce in the queue.
Review fix (memory): as first committed (`876060d`) the writer reserved a
whole 72 KiB batch on its first write and kept it across small flushes, so
every idle follow reader (one `NetworkWriter` per file read) held 72 KiB for
the whole session: 1000 writers doing 3 one-line write+flush cycles retained
70.6 MiB after GC, against 0.3 MiB at `f290a31` (about +3.6 MiB with the
default 50 concurrent tails). The first memory fix (`5a9ac06`) reserved the
batch only after a full-batch hand-over and dropped the reservation on the
next small flush. That regressed follow-mode catch-up, which flushes after
every read chunk: a chunk whose formatted output exceeds 64 KiB hands one
batch over and flushes a small remainder, so every chunk regrew a buffer from
zero, handed it over, allocated a fresh 72 KiB and dropped it again at the
remainder flush. It also charged too much: a naturally grown batch crossing
64 KiB usually has a 128 KiB `bytes.Buffer` backing, which the queue adopted
and charged in full (131,072 B charged for about 65.6 KB of payload with
99-byte lines; the unit tests used 127-byte lines, which fill exactly
64 KiB, and missed it).

Final design (second review fix): a writer that is not reserved grows its
buffer naturally; once its batch reaches 24 KiB (three eighths of the flush
threshold, while the natural buffer still has its 32 KiB capacity) the
pending bytes are moved into an exact 72 KiB allocation and the writer
becomes reserved, so a batch of lines up to 8 KiB that crosses 64 KiB is
never regrown and is handed over, and charged, at 72 KiB (longer lines: see
the third review fix below). After a hand-over a reserved
writer allocates the next 72 KiB on its next write. A small flush copies its
bytes out and resets the buffer but keeps the reservation; the reservation
is dropped, and the writer leaves reserved mode, only after 3 consecutive
small flushes (under 24 KiB) with no hand-over or larger batch in between.
Catch-up has at most one small remainder per hand-over, so it keeps its
reservation and fills the same buffer across chunks; 3 rather than 2 also
tolerates one short read between full ones. Measured with the reviewer's
benchmark (`f290a31` / `7a16500` / `5a9ac06` / final, 3 runs of 2000
iterations each, ns/op ranges): follow catch-up in protocol format, 655
99-byte lines per chunk, 269-285 us 216 KB/op / 213-253 us 142 KB/op /
360-375 us 333 KB/op / 218-246 us 142 KB/op, retained/payload 1.000 / 1.083
/ 1.335 / 1.083; plain 70 KiB chunks 170-193 us 161 KB/op / 105-117 us 87 KB/op
/ 310-331 us 348 KB/op / 107-130 us 87 KB/op, largest charged entry 65,600 /
73,728 / 131,072 / 73,728 B; alternating 40K/8K flushes 59 / 47 / 112 / 47
KB/op. Bulk 1 MiB reads with 99-byte lines: 2.6-2.7 ms / 1.47-1.55 ms /
1.42-1.55 ms / 1.42-1.55 ms, largest charged entry 65,600 / 73,728 /
131,072 / 73,728 B. The final design matches `7a16500` on these paths.
Retained heap after GC for 1000 writers: idle writers (one-line
write+flush cycles only) 0.3 / 70.6 / 0.3 / 0.4 MiB. Writers that caught up
(three 70 KB chunks, each flushed) and then went idle: `f290a31` keeps its
naturally grown 128 KiB buffers for good (125.3 MiB), `7a16500` 70.6 MiB,
`5a9ac06` 0.3 MiB, and the final design 70.6 MiB until two more small
flushes after the catch-up remainder, then 0.4 MiB. So a follow reader that
catches up and then receives nothing keeps 72 KiB (still less than
`f290a31`'s 128 KiB) until it sees further output. Server-mode dcat (100 MiB, four
dservers side by side, 16 rounds rotating the order) dserver CPU median /
range: `f290a31` 1.06 / 0.92-1.10 s, `7a16500` 0.77 / 0.73-0.88 s,
`5a9ac06` 0.78 / 0.71-1.71 s, final 0.79 / 0.72-0.88 s, `cmp` identical
every run: the bulk gain holds (ranges overlap between the last three).
Review fix 3 (charging): at `d38aea8` a line longer than 8 KiB made the
batch crossing 64 KiB overflow the 72 KiB reservation, so `bytes.Buffer`
regrew it (to 144 KiB for 9-12 KiB lines) and the queue adopted and charged
that whole buffer: charged/payload (protocol format) 2.00 for 9-12 KiB lines, 1.80 for
20-40 KiB, 1.43 for 100 KiB, and the default 2 MiB cap held only about
1.03 MB of payload before backpressure for 9-12 KiB lines (1.15 MB for
20-40 KiB, 1.43 MB for 100 KiB; plain format; f290a31: 2.05-2.06 MB). A flush of
32 KiB to 64 KiB was likewise adopted in its 72 KiB allocation (charged up to
2.25 times its payload at the 32 KiB bound, 1.8 for 40 KiB flushes). Also, at the smallest
`OutputBufferMaxBytes` the handler accepts (`MaxLineLength` plus two 64 KiB
batches) the first batch was adopted at 73,728 B and the second then fit
neither adopted nor copied, so the queue held one batch instead of two (not a
deadlock). The validated minimum was not raised, because configs that are
valid today would then be rejected. Final rule: the queue adopts a payload of
at least 32 KiB only when its spare capacity is at most an eighth of its
length (`outputAdoptable`, so an adopted payload is charged at most 1.125
times its bytes) and the budget left after adopting still holds another
payload of the same length (relaxed by review fix 4 below); otherwise it copies at exact length when that
fits. The writer hands over only a batch that passes the same spare-capacity
check and copies any other batch at exact length; a buffer regrown past
72 KiB is kept while large batches keep coming from it (a stream of long
lines refills it without regrowing) and dropped on a small flush. Measured
with the reviewer's overlay test (writer into queue, lines of 9 / 12 / 20 /
40 / 100 KiB, protocol format): charged/payload 1.108 / 1.109 / 1.099 /
1.099 / 1.040 (`d38aea8` 1.995 / 1.996 / 1.798 / 1.799 / 1.430; the long-line
batch copy lands in an 80-104 KiB size class, which the queue charges); 99-byte
lines stay at 1.124 (a full batch in 72 KiB). Payload queued before
backpressure at the default 2 MiB cap (plain format): 99-byte lines 1,836,800 B (28 batches,
unchanged since `876060d`; `f290a31` 2,033,600 B), 9 / 12 / 20 / 40 / 100 KiB
lines 1,843,400 / 1,843,350 / 1,884,252 / 1,884,206 / 1,945,619 B (`d38aea8`
1,032,304 / 1,032,276 / 1,146,936 / 1,146,908 / 1,433,614 B; `f290a31`
2,048,020-2,064,608 B). At the minimum cap for a 1 KiB `MaxLineLength` (132,096 B) two
batches of 99-byte lines queue again (131,200 B in plain format, both copied), as at
`f290a31`. The reviewer's writer/queue benchmark (3 runs of 2000 iterations,
final / `d38aea8` / `f290a31`): follow catch-up in protocol format 187-222 /
201-252 / 266-294 us/op, 142 / 142 / 216 KB/op, retained/payload 1.083 /
1.083 / 1.000; bulk 1 MiB of 99-byte lines 1.46-1.53 / 1.47-1.49 /
2.38-2.50 ms/op; plain 70 KiB chunks 109-136 / 103-110 / 160-178 us/op,
largest charged entry 73,728 / 73,728 / 65,600 B. Constant 40 KiB flushes
are copied again: 100-106 us/op, 99 KB/op, retained/payload 1.000 (`d38aea8`
79-90 us/op, 74 KB/op, 1.798; `f290a31` 92-109 us/op, 99 KB/op, 1.000).
These benchmarks never reach the cap, so they missed that the fix 3 rule
("the budget left after adopting still holds another payload of the same
length") copies about half of all batches under backpressure: when a slow
client frees one batch's worth of room, the queue has room for this batch but
not another one after it, adopted or copied, so the rule fell back to a copy
that saved nothing. In server-mode dcat (100 MiB) an instrumented `0acf3c3`
dserver adopted only 738-899 of 1,611 batches and copied the rest, and the
dserver CPU median rose from 77-78 ticks (`d38aea8`) to 83 (85 in the
review's own run; `f290a31` about 106), about a quarter
of the hand-over gain; the "bulk gain kept" statement for fix 2 did not hold
after fix 3.
Review fix 4 (backpressure): the queue now adopts an adoptable payload
whenever its capacity fits and adoption does not cost the next same-length
batch room that a copy would have left it (`adoptionKeepsNextBatchRoom`:
the next batch fits after adopting, or it would not fit after a copy either).
The spare capacity only matters in the narrow band where a copy leaves room
for the next batch and adoption does not; there the payload is still copied,
so at the minimum accepted cap two batches still queue (both copied, as in fix
3). Instrumented server-mode dcat: 1,611 of 1,611 batches adopted. The new
deterministic unit test (writer blocked on a full 2 MiB queue, reader
reading 32 KiB only then) adopts 219 of 219 (protocol format) and 167 of 167
(plain) batches, against 0 of 215 and 0 of 163 with the fix 3 rule. dserver
CPU over 32 rotating rounds, median ticks: `0acf3c3` 83, `d38aea8` 77,
fix 4 79.5 (ranges 73-100 / 72-102 / 72-96, overlapping; `cmp` identical
every run), so the gain is back to within noise of `d38aea8`.
The session generation is now an
`atomic.Uint64`, still written under the session mutex and read per line
without a lock; its own row above shows no measurable effect end to end, so
the dcat gain comes from removing the copies. The atomic read also removes the
output-lock to session-lock ordering edge that `tryRead` had. Follow mode is
measured only at the writer and queue level (benchmark above), not end to
end; the 1 GiB input was not measured.

`65` notes (decision: no change). Both intervals stay at 100 ms: the server
EOF poll in `followLineProcessor.handleReadError` and
`stdoutIdleFlushInterval` in the client stdout sink. `fileIdleFlushInterval`
was not changed or measured: it only bounds when the daily log file is
written, while the terminal latency of the default `fout` logger comes from
its stdout sink.
Variant binaries were built with `go build` from `b3a84a9` with only the two
constants changed (S = server poll, C = client flush, 100 / 50 / 20 ms). A
scratch harness started one dserver (`--cfg none --logger stdout`) and 8
`dtail --plain --logger stdout` follow sessions on separate empty files, waited
until every session was live, read dserver and client CPU from `/proc` over
20 s of idle following, then appended 40 single probe lines to session 0's file
at random 250-450 ms spacing and timed each until it appeared on the client's
stdout pipe. Variants alternated order per round. `sv_dtail_follow` of
`benchmarks/upstream_vs_local_bench.sh` was not used for the decision: it
times a burst with 20 ms marker polling, so it measures burst throughput, not
the per-line floor.
The latency floor is as expected from two independent 100 ms timers: mean
about 100 ms, worst about 200 ms. Shortening either interval lowers latency in
proportion, but idle CPU rises with the wakeup rate: 50 ms roughly doubles
both dserver idle CPU with 8 sessions and client idle CPU per session, 20 ms
about triples dserver and more than quadruples the client. Even at the current
setting a dserver with 8 idle follow sessions uses 6-9% of a core on this
bhyve/hpet VM. A 20 s dserver CPU profile of that state (8 sessions really
following their files, S100) holds 780 ms of samples: 35% under
`followLineProcessor.handleReadError` (the EOF poll path), including 15% of all
samples in the `ReadFile.truncated` check (seek, fstat and lstat on every
poll), and 32% in
`runtime.nanotime`, which is expensive on this hpet clocksource. So the idle
cost is the per-wakeup work of the poll plus clock reads, and it scales with
the wakeup rate. The task's
keep rule (clear latency gain and no measurable idle CPU rise) is therefore
not met by any shorter interval. Latency could be lowered without extra
wakeups by event-driven designs, not evaluated here: flushing the client
stdout buffer when the receive loop has drained its input instead of on a
ticker, and inotify-driven wakeups instead of the server EOF poll.
Other observations from the review, not acted on (possible follow-ups):
by the reviewer's measurement a dserver with no sessions (`--logger stdout`)
uses about 1.2% of a core from the two server logger 100 ms idle-flush tickers,
which wake even with an empty buffer (0 with `--logger none`); arming the timer
only on the first buffered write would remove that. In follow mode
`handleReadError` already runs `truncated()` on every 100 ms EOF poll, so the
per-file `periodicTruncateCheck` goroutine and its 3 s ticker are redundant for
follow reads. The harness clients used `--logger stdout`; the default `fout`
client adds a file sink with its own 100 ms ticker, so default-config client
idle CPU is somewhat higher than recorded above. A 16-session attempt failed at
the 11th session; that is expected, because `MaxConnections` defaults to 10
(`internal/config/server.go`) and the server closes further connections.
Only `make clean && make build` was run for this change (docs only).

`75` notes (closure, 2026-09-21): all nine plan tasks (`y4`, `z4`, `05`, `15`,
`25`, `35`, `45`, `55`, `65`) are done. The full suite (`make clean && make
build && make test && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test && make vet
&& make lint`) exited 0 at `df52a1e` and `gofmt -l .` reported nothing.
`benchmarks/upstream_vs_local_bench.sh smoke` passed, then `run --iterations 5`
ran twice, back to back on this host (4 vCPUs, go1.26.6, page cache dropped
before every observation, implementations interleaved): once against upstream
(`../dtail-upstream` at `91d3500`) and once against the pre-plan commit
`c472f83` passed via `--upstream-root`. Every scenario recorded 5 samples and
0 failures. The rows above compare against `c472f83`, which isolates what this
plan changed; ranges are min-max over the 5 samples. The clear wins are server
mode dcat (8.67 s to 0.64 s median, the target of `y4`, `z4`, `05` and `55`),
the dmap scenarios (2.1-2.3x by median, the target of `15`, `25` and `35`),
server dgrep high-match (2.8x) and the follow burst (4.6x). Serverless dcat
(the target of `45`) improved by median, 1.28x on 1 GiB (1.87 s to 1.47 s,
ranges clearly apart) and 1.31x on 100 MiB (0.237 s to 0.181 s, but the ranges
overlap because of one slow current sample, 0.273 s in iteration 1; the
pre-plan run's iteration 1 was slow too, at 0.397 s). The two dgrep low-match
scenarios show no measurable change: their before and after ranges overlap
(serverless median 0.195 s before versus 0.210 s after, server 0.301 s versus
0.308 s), which fits the workload: few lines match, and most of the plan's read-path
changes act on matched lines. Against upstream the same build is 9.5x (server
dgrep low-match) to 184x (serverless dcat 1 GiB) faster; those numbers are in
`BENCHMARK_RESULTS.md`. A Python `ConnectionRefusedError` traceback in the run
log is the script's port-readiness probe polling before dserver listens; it
retries and no observation was lost. Raw results, reports and binaries' SHA-256
sums were kept in the session's scratch directory, not committed.
