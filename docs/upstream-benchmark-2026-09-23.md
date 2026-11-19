# DTail versus Mimecast upstream — 2026-09-23

The local fork completed every measured workload faster on this host. This
includes real SSH client/server runs, not just serverless operation. All 110
observations succeeded: 11 scenarios, five observations per implementation.
These are host-specific end-to-end results, not universal speedup claims or
an attribution of the gains to the most recent changes alone.

A later re-run on another host with newer revisions is in
[upstream-benchmark-2026-11-11.md](upstream-benchmark-2026-11-11.md).

## Revisions and method

- Upstream: `../dtail-upstream`, Mimecast DTail commit
  `91d35001488dd036b1f1def30cc435d3ab25c5f1`. This is the supplied checkout;
  no fetch was performed and it is not claimed to be the latest remote commit.
- Local: `5eece8df07bfbbb6c6dcfe921ab26ea8490cbdba`. Both tracked trees were
  clean at build time. The unrelated, untracked code-quality audit was untouched.
- Host: Rocky Linux 9 bhyve guest, Intel N100, four vCPUs, Linux
  `5.14.0-687.42.1.el9_8.x86_64`, clocksource `hpet`.
- Both built with Go 1.26.6 linux/amd64, `-p=1 -trimpath -buildvcs=false`;
  runtime `GOMAXPROCS=4`. No PGO generation or PGO build was requested.
- Existing [comparison harness](../benchmarks/upstream_vs_local_bench.sh),
  deterministic identical inputs, one observation at a time, alternating
  upstream/local order across rounds, with untimed warm-ups.
- Caches were dropped before every observation. The archive records 114
  successful drops: 110 timed observations plus four follow smoke/warm-up runs.
  This clears guest caches, not the hypervisor's storage cache; follow measures
  newly appended data, not a cold read of the whole fixture from disk.
- Both clients use `--logger stdout --plain --noColor --cfg none`. Timed
  cat/grep/map output goes to `/dev/null`; follow output goes to a file.
- Server mode uses loopback SSH: upstream client with upstream `dserver`,
  local client with local `dserver`. Both servers remain running during that
  phase; only one benchmark client runs at a time.
- No competing Codex build/test process was found, so no agent needed pausing.

## Elapsed results

Medians in seconds; speedup is upstream median divided by local median.
Inputs are 100 MiB except the 1 GiB cat and 10 MiB follow burst, rounded to
whole records. Grep low matches `user999 `; grep high matches `ERROR`.

| Mode | Workload | Upstream | Local | Speedup |
|---|---|---:|---:|---:|
| Serverless | Cat, 100 MiB | 26.423370 | 0.222603 | 118.702× |
| Serverless | Cat, 1 GiB | 265.513664 | 1.461377 | 181.687× |
| Serverless | Grep, low matches | 2.672901 | 0.229963 | 11.623× |
| Serverless | Grep, high matches | 2.801816 | 0.212176 | 13.205× |
| Serverless | Map aggregate | 33.098953 | 0.876127 | 37.779× |
| SSH server | Cat, 100 MiB | 31.555389 | 0.653028 | 48.322× |
| SSH server | Grep, low matches | 2.885048 | 0.320782 | 8.994× |
| SSH server | Grep, high matches | 3.572905 | 0.357800 | 9.986× |
| SSH server | Map count | 31.551966 | 0.757769 | 41.638× |
| SSH server | Map aggregate | 31.970000 | 0.930349 | 34.363× |
| SSH server | Follow, 10 MiB burst | 3.987595 | 0.257378 | 15.493× |

For scale, all five SSH cat observations were 31.124–31.656 s upstream and
0.619–0.681 s local. Follow observations were 3.890–4.088 s upstream and
0.252–0.281 s local. Serverless aggregate varied more upstream (30.445–37.373 s)
than local (0.850–0.899 s). Five samples are descriptive, not a confidence
interval. Every individual timing is in the linked raw results below.

Map count uses `from STATS select count($line) group by $hostname`.
Aggregate uses
`from STATS select count($line),avg($goroutines),max($goroutines),sum($goroutines) group by $goroutines`.

## CPU and memory

CPU medians include client user+system time and, in server mode, the measured
`dserver` CPU delta. SSH cat used 96.613 CPU seconds upstream versus 1.712 local;
SSH follow used 11.700 versus 0.370. These are CPU seconds, not wall seconds.

The local server is not smaller in every case: the SSH cat server RSS snapshot
median was 21,536 KiB upstream versus 27,576 KiB local. Client peak RSS for that
case was 21,804 versus 22,776 KiB. The summary includes all CPU/RSS medians;
server RSS is a current snapshot, not a peak, and follow has no client peak-RSS
measurement. Long-lived-server snapshots can reflect preceding workloads.

## Correctness and interpretation

- Untimed correctness checks passed in both modes: cat/grep output comparison,
  canonicalized MapReduce output and count checks, and follow record checks.
- All timed observations reported `ok`; none timed out. The startup
  `ConnectionRefusedError` messages in the run log come from retrying the port
  readiness probe, not from failed timed observations.
- Additionally, all ten full-size follow outputs were compared byte-for-byte
  with the 95,326-record fixture after removing probe/end markers. All matched.
  Timed cat/grep/map output was discarded, so full-size byte parity is not
  asserted for those timed runs; their correctness checks use smoke fixtures.
- The very large ratios are specific to this VM and these workloads. Prior
  profiling identified costly per-line clocks on this HPET host; this is
  relevant context, not a new causal profile from this run. Results on a
  TSC-backed host or over a real network can differ substantially.
- Follow timing starts at append and ends when the harness detects the final
  marker, including append and polling overhead. It is burst delivery time,
  not per-line latency. The harness uses `--max 2147483647`, which selects
  private follow reads in the local server; it does not measure shared fan-out.
- This run does not cover payload file teeing, shared scheduled reads,
  concurrent-client scaling, sparse follow latency, rotation, compression,
  high-cardinality result sorting, or idle CPU. It compares whole checkouts,
  not just the recent optimization commits.

## Reproduction and evidence

Run on an otherwise idle host with the existing privileged cache-drop helper:

```bash
GOMAXPROCS=4 bash benchmarks/upstream_vs_local_bench.sh run \
  --upstream-root ../dtail-upstream \
  --local-root . \
  --workdir "$(mktemp -d /tmp/dtail-upstream.XXXXXXXX)" \
  --iterations 5
```

The [compact evidence archive](benchmarks/upstream-2026-09-23/README.md) contains
all 110 raw observations, summary metrics, revisions and binary/fixture hashes,
cache-drop receipts, and the run log. It excludes keys, binaries and payloads.

## Historical results

Older standalone benchmark reports, baseline outputs and the earlier follow-up
evidence archive were removed from the working tree to leave one current
comparison. They remain recoverable from Git commit
`5eece8df07bfbbb6c6dcfe921ab26ea8490cbdba`; for example:

```bash
git show 5eece8d:docs/performance-followup-2026-09-23.md
git show 5eece8d:docs/shared-reads-benchmark-2026-09.md
git show 5eece8d:BENCHMARK_RESULTS.md
```

The earlier local-before/after and shared-read results test different questions;
this upstream comparison does not replace that historical evidence. Benchmark
code, harnesses and implementation plans/notes are retained.
