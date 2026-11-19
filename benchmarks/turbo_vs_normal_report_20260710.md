# Turbo vs Normal (Non-Turbo) Benchmark Report — 2026-07-10

**Question:** After the turbo-path rework (commits `6189e6a..6384365` plus
`fec2f9d`/`e76e0e6`: turboManager mutex, EOF-handshake epoch guard, Read
remainder buffering, ctx-aware sends, stats fixes) — is turbo mode still
faster than the normal (channel-based) path?

**Answer: Yes — turbo remains dramatically faster everywhere it applies and
works (2x–24x). No performance regression from the rework was found. However,
the benchmark uncovered a functional deadlock: server-mode `dmap` with turbo
enabled (the default!) never terminates the client session. This bug is
pre-existing (reproduces at `03c5cfb`, before the rework window) and has been
masked because integration tests force-disable turbo.**

## Methodology

- Binaries rebuilt from clean at HEAD `e76e0e6` (`make clean && make build`).
- Turbo is default-on; non-turbo runs set `DTAIL_TURBOBOOST_DISABLE=yes` on
  the process that runs the server handlers (the client itself in serverless
  mode, `dserver` in server mode).
- Serverless scenarios: client binaries run directly against local files.
- Server scenarios: two `dserver` instances on localhost (one turbo, one
  non-turbo, `--cfg none`, real SSH auth via a dedicated RSA key +
  `CacheDir` authorized_keys) — *not* `DTAIL_INTEGRATION_TEST_RUN_MODE`,
  because that mode force-disables turbo (`internal/config/initializer.go:98`).
- Turbo/non-turbo iterations alternate A/B to spread thermal/load drift
  (applies to the dcat/dgrep/dmap scenarios; the dtail follow scenario runs
  all turbo iterations first, then all non-turbo, and the server-mode dmap
  rows were hand-run — see the provenance note below).
  1 warmup + 5–7 measured iterations per scenario/mode; medians reported.
- Wall-clock timing, client stdout to /dev/null (except dtail follow).
- Datasets: `benchmarks/testdata/medium.log` (100 MB, 1.22 M lines),
  `large.log` (1 GB), and a generated 100 MB `MAPREDUCE:STATS` file
  (800 k lines, 50 hostnames). dgrep patterns: `"user999 "` (903 hits,
  ~0.07 %) and `"ERROR"` (235 k hits, ~19 %).
- dtail follow: client follows an empty file over SSH; after the stream is
  established (probe line round-trip), a 10 MB / 100 k-line burst plus end
  marker is appended; time from append to marker arrival at the client is
  measured, and delivered-line completeness is counted.
- Machine: i7-1185G7 (8 threads), Fedora, Go 1.26.4. Caveat: laptop on
  battery (balanced profile), so absolute numbers are noisy; relative
  turbo/non-turbo ratios are the meaningful signal.
- Non-turbo serverless runs go through an `env` wrapper that turbo runs do
  not (~1 ms extra process spawn). Negligible at these magnitudes, and it
  biases against turbo, not for it.
- Raw data: `turbo_vs_normal_results_20260710.csv` (committed; an identical
  copy sits in the gitignored `benchmark_results/turbo_comparison_20260710.csv`).
  Harness: `turbo_vs_normal_bench.sh`.
- Provenance of the server-mode dmap rows: they were *not* produced by the
  committed harness in one pass. The original suite run deadlocked on the
  turbo dmap warmup (that incident is the finding); after diagnosing it
  (SIGQUIT goroutine dump of the hung dserver), the non-turbo dmap timings
  (5 consecutive iterations per query) were re-run by hand against a fresh
  non-turbo dserver, and the turbo rows are hand-annotated hang markers.
  The hang itself was independently re-verified on fresh servers (60 s and
  30 s timeouts, 100 MB and 132 KB inputs) and at commit `03c5cfb`. The
  committed harness wraps dmap runs in `timeout 120`, so a fresh run records
  the turbo halves as `failed-rc124` rows instead of blocking the suite.

## Results (median wall time, seconds)

### Serverless mode

| Scenario                    | Turbo  | Non-Turbo | Speedup | Verdict |
|-----------------------------|--------|-----------|---------|---------|
| dcat 100 MB                 | 0.423  | 4.343     | 10.3x   | turbo much faster |
| dcat 1 GB                   | 4.285  | 44.683    | 10.4x   | turbo much faster |
| dgrep 100 MB, low hit rate  | 0.206  | 4.988     | 24.2x   | turbo much faster |
| dgrep 100 MB, high hit rate | 0.258  | 5.389     | 20.9x   | turbo much faster |
| dmap 100 MB (control)       | 4.936  | 4.995     | 1.0x    | expected: turbo N/A serverless |

Turbo throughput: ~236 MB/s dcat, ~390–485 MB/s dgrep, vs ~19–23 MB/s
non-turbo.

### Server mode (localhost SSH)

| Scenario                    | Turbo        | Non-Turbo | Speedup | Verdict |
|-----------------------------|--------------|-----------|---------|---------|
| dcat 100 MB                 | 9.736        | 9.843     | 1.0x    | neutral: SSH transport/client-bound (~10 MB/s both) |
| dgrep 100 MB, low hit rate  | 0.893        | 5.743     | 6.4x    | turbo much faster |
| dgrep 100 MB, high hit rate | 2.373        | 3.552     | 1.5x    | turbo faster |
| dmap count, 100 MB stats    | **DEADLOCK** | 5.711     | n/a     | **turbo functionally broken** |
| dmap group-by-agg, 100 MB   | **DEADLOCK** | 4.381     | n/a     | **turbo functionally broken** |
| dtail follow 10 MB burst    | 0.63, **100 % delivered** | lossy: only ~38–41 % delivered; completion unbounded* | n/a | turbo strictly better |

\* Non-turbo tail drops lines when the consumer lags (documented behavior,
`internal/io/line/line.go`). In 1 of 3 runs the end marker itself was dropped
and the run never completed (>90 s). When the marker survived, "completion"
in ~0.4 s simply reflects that ~60 % of the data was discarded. Turbo
delivered all 100,000 lines in ~0.63 s in every run.

## Per-tool verdicts

- **dcat**: turbo still a big win serverless (10x). In server mode over SSH
  the whole-file transfer bottlenecks the session (~10 MB/s) and turbo is
  neutral — no benefit, no harm.
- **dgrep**: turbo still a big win everywhere (1.5x–24x; larger when the
  server-side read dominates, i.e. low hit rates).
- **dtail**: turbo is strictly better — complete delivery at ~16 MB/s vs
  lossy (~40 %) delivery on the normal path.
- **dmap (serverless)**: turbo not applicable; parity confirmed.
- **dmap (server mode)**: turbo aggregation itself is fast — the client
  *receives correct results* within seconds — but the session never
  terminates and the client hangs until killed. Functionally broken in the
  default configuration.

## Anomaly: server-mode turbo dmap deadlock (feeds task ss0)

Reproduction: 100 % reproducible, any file size (tested 132 KB and 100 MB):

```
dserver --cfg none --port P &                # turbo on by default
dmap --servers localhost:P --query "from STATS select count($line) group by $hostname" \
     --files stats.log                        # prints results, never exits
DTAIL_TURBOBOOST_DISABLE=yes dserver ...      # same query completes in ~6 s
```

Deadlock cycle (from SIGQUIT goroutine dump of the hung dserver, saved during
analysis):

1. `handleMapCommand` starts `mapCommand.Start` →
   `TurboAggregate.Start` blocks in `select { <-ctx.Done(); <-a.done.Done() }`
   (`internal/mapr/server/turbo_aggregate.go:156`). The map command counts as
   an active command the whole time.
2. When all files finish, `shutdownCoordinator.onFileProcessed`
   (`internal/server/handlers/shutdown_coordinator.go:22`) requires
   `activeCommands == 0` to call `finalizeWhenIdle()` — but the map command
   is still active, so finalization never runs.
3. `TurboAggregate.done` is only shut down via `baseHandler.Shutdown()`
   (session teardown) — which only happens when the client disconnects.
   Nothing signals "input exhausted" to the turbo aggregate.
4. The read command's EOF epilogue explicitly excludes the turbo-aggregate
   path (`internal/server/handlers/readcommand.go:188` — the
   "never-signaling-joiner" comment), and
   `readcommand.go:240` says "The aggregate will handle channel closure when
   it's done" — but no such trigger exists for `TurboAggregate`.
   (The regular `Aggregate` finishes because its dedicated lines channel is
   closed at EOF, which drives flush/serialize/return.)

Why unnoticed:
- `DTAIL_INTEGRATION_TEST_RUN_MODE=yes` force-disables turbo
  (`initializer.go:98`), so *all* integration tests exercise only the
  non-turbo server path.
- Several tests/scripts still set `DTAIL_TURBOBOOST_ENABLE=yes`, an env var
  the config has not read since turbo became default-on (`aa2f547`,
  2025-07-04) — e.g. `integrationtests/dcat_test.go:285`,
  `integrationtests/dmap_test.go:387`, `benchmarks/turbo_comparison.sh`,
  `benchmarks/dcat_direct_benchmark_test.go`. They silently test nothing.
- Bug is pre-existing: reproduced at `03c5cfb` (parent of the recent turbo
  rework window). The rework did not introduce it.

Suggested direction for ss0 (not implemented here): give
`TurboAggregate` an input-exhausted signal analogous to the regular
aggregate's channel close — e.g. have `shutdownCoordinator` (or the read
command's map branch) call a `FinishInput()` that performs
final-flush + serialize + `done.Shutdown()` once `pendingFiles == 0`, instead
of gating on `activeCommands == 0` which the blocked map command itself keeps
non-zero.

Secondary observations (also candidates for ss0/follow-ups):
- Server-mode dcat of full files is transport-bound; if server-mode dcat
  matters, the win would come from the SSH writer path, not the reader.
- MapReduce dynamic key=value fields did not resolve via `$field` queries
  against the dtail native log format in ad-hoc testing
  (`avg($currentConnections)` = 0 while positional `$goroutines` works);
  worth a correctness check, unrelated to turbo.
- The dmap client parses the server's `AUTHKEY OK` acknowledgement as
  aggregate data, producing a cosmetic ERROR line in both modes
  (tracked as task zs0).
