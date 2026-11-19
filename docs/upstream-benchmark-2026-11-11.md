# DTail versus Mimecast upstream — 2026-11-11

The local fork completed every measured workload faster again, in serverless
mode and over loopback SSH. All 110 observations succeeded: 11 scenarios, five
observations per implementation. These are host-specific end-to-end results,
not universal speedup claims. This is a re-run of the
[2026-09-23 comparison](upstream-benchmark-2026-09-23.md) on a different host
with newer revisions on both sides, so the two reports are not directly
comparable (see "Comparison with 2026-09-23").

## Revisions and method

- Upstream: `../dtail-upstream`, Mimecast DTail `master` at commit
  `d2425e8261d388f1dd2b80ef29cae9308612dffc` (2026-09-21), freshly cloned from
  `https://github.com/mimecast/dtail.git`. The previous report used
  `91d35001488dd036b1f1def30cc435d3ab25c5f1`.
- Local: `3b2ec4ce1b74e6c66a24c221f6d476ccd75f8caf` (`fix: bug sweep across
  mapr, io, readhub, jobs, server and clients`), measured from a detached
  `git worktree` of `HEAD`, which pins the measured source to that commit
  while the main working tree stays free for other work. Both measured trees
  were clean (the harness refuses to run on tracked changes).
- Host: Fedora 44 on bare metal, 8 logical CPUs, Linux
  `7.2.15-200.fc44.x86_64`, clocksource `tsc`, CPU governor `performance`,
  home filesystem on LUKS-encrypted NVMe. The previous run used a four-vCPU
  Rocky Linux 9 VM with an `hpet` clocksource.
- Both built with Go 1.26.10 linux/amd64, `-p=1 -trimpath -buildvcs=false`;
  runtime `GOMAXPROCS=4` (to match the earlier four-vCPU run).
- Harness: [`benchmarks/upstream_vs_local_bench.sh`](../benchmarks/upstream_vs_local_bench.sh),
  unchanged. Deterministic identical inputs, one observation at a time,
  alternating upstream/local order, untimed warm-ups, `--iterations 5`.
  Caches were dropped before every observation (114 receipts archived).
- Both clients use `--logger stdout --plain --noColor --cfg none`. Timed
  cat/grep/map output goes to `/dev/null`; follow output goes to a file.
- Server mode uses loopback SSH with a matched client and dserver per
  implementation, both servers running while only one client runs.
- The harness calls `sudo -v` to cache credentials, which needs a TTY. The run
  had no TTY but passwordless `sudo -n`, so a `sudo` wrapper that turns `-v`
  into a no-op and forwards every other call to the real `sudo` was put first
  on `PATH`. The repository and harness were not modified.
- The host was not idle when the run started (load average about 2.3 from
  other work), unlike the previous run's "otherwise idle" requirement. The
  harness itself ran alone (about eight minutes), but background activity on
  the desktop was not controlled or recorded.

## Elapsed results

Medians in seconds; speedup is upstream median divided by local median.
Inputs are 100 MiB except the 1 GiB cat and 10 MiB follow burst. Grep low
matches `user999 `; grep high matches `ERROR`.

| Mode | Workload | Upstream | Local | Speedup | Local MiB/s |
|---|---|---:|---:|---:|---:|
| Serverless | Cat, 100 MiB | 2.709376 | 0.156172 | 17.349× | 640 |
| Serverless | Cat, 1 GiB | 26.018256 | 1.199818 | 21.685× | 853 |
| Serverless | Grep, low matches | 1.085344 | 0.142000 | 7.643× | 704 |
| Serverless | Grep, high matches | 1.161523 | 0.117640 | 9.874× | 850 |
| Serverless | Map aggregate | 6.139612 | 0.659756 | 9.306× | 152 |
| SSH server | Cat, 100 MiB | 7.640945 | 0.262316 | 29.129× | 381 |
| SSH server | Grep, low matches | 1.408317 | 0.161945 | 8.696× | 617 |
| SSH server | Grep, high matches | 1.989834 | 0.163361 | 12.181× | 612 |
| SSH server | Map count | 6.195546 | 0.540084 | 11.471× | 185 |
| SSH server | Map aggregate | 6.255707 | 0.673898 | 9.283× | 148 |
| SSH server | Follow, 10 MiB burst | 0.812443 | 0.155260 | 5.233× | 64 |

Ranges of the five observations, for scale: the upstream serverless 1 GiB cat
varied most (18.3–26.5 s; local 0.93–1.26 s), upstream SSH cat was 7.2–8.1 s
(local 0.23–0.27 s), upstream follow 0.76–1.15 s (local 0.14–0.16 s). In
every scenario the slowest local observation was faster than the fastest
upstream one. Five samples are descriptive, not a confidence interval; every
observation is in the archived `results.csv`.

Map count uses `from STATS select count($line) group by $hostname`; aggregate
uses `from STATS select count($line),avg($goroutines),max($goroutines),sum($goroutines) group by $goroutines`.

## CPU and memory

CPU medians include client user+system time and, in server mode, the measured
`dserver` CPU delta. RSS is the median client peak RSS and, in server mode, a
`dserver` RSS snapshot taken after each observation (not a peak).

| Workload | CPU s upstream | CPU s local | Client RSS KiB (up/local) | Server RSS KiB (up/local) |
|---|---:|---:|---:|---:|
| Serverless cat, 100 MiB | 5.564 | 0.119 | 17,484 / 16,200 | — |
| Serverless cat, 1 GiB | 54.545 | 1.108 | 17,644 / 16,012 | — |
| Serverless grep, low | 1.455 | 0.110 | 17,208 / 16,164 | — |
| Serverless grep, high | 1.725 | 0.085 | 17,088 / 16,276 | — |
| Serverless map aggregate | 6.091 | 0.636 | 17,992 / 16,248 | — |
| SSH cat, 100 MiB | 19.839 | 0.750 | 17,776 / 18,248 | 18,388 / 19,628 |
| SSH grep, low | 1.991 | 0.133 | 16,264 / 16,152 | 19,352 / 20,320 |
| SSH grep, high | 5.004 | 0.192 | 17,364 / 17,652 | 18,764 / 19,852 |
| SSH map count | 5.872 | 0.514 | 16,296 / 16,480 | 19,420 / 21,404 |
| SSH map aggregate | 5.992 | 0.653 | 16,284 / 16,184 | 19,020 / 22,216 |
| SSH follow, 10 MiB | 2.060 | 0.060 | — | 19,008 / 20,032 |

The local fork uses far less CPU. Memory is a wash on the client; the local
dserver snapshot is 1–3 MiB larger than upstream's in every SSH scenario
(largest difference: map aggregate, 22.2 versus 19.0 MiB). Follow has no
client peak-RSS measurement.

## Correctness and interpretation

- Untimed correctness checks passed in both modes before timing: cat/grep
  output comparison, canonicalized MapReduce output and count checks, and
  follow record checks.
- All 110 timed observations reported `ok`; none timed out.
- All twelve follow outputs (five timed and one warm-up per implementation)
  were compared byte-for-byte with the 10 MiB follow fixture after removing
  the probe and end marker lines. All matched. Timed cat/grep/map output was
  discarded, so full-size parity there rests on the smoke-fixture checks.
- The first smoke attempt failed once with an upstream serverless `dcat` that
  exited non-zero with an empty stderr. It could not be reproduced: the same
  command succeeded when run by hand and the smoke test then passed on retry
  and again before the timed run. The cause was not identified. It may have
  been a first-run effect while the machine was busy building; this is
  unverified. All timed observations were unaffected.
- The follow scenario uses `--max 2147483647`, so it measures burst delivery
  time over a private (not shared) read, not per-line latency, exactly as in
  the previous report.
- This run does not cover payload file teeing, shared scheduled reads,
  concurrent-client scaling, sparse follow latency, rotation, compression,
  high-cardinality result sorting, or idle CPU. It compares whole checkouts,
  not individual optimizations.

## Comparison with 2026-09-23

The speedups here are smaller than before: SSH cat 29× versus 48×, serverless
1 GiB cat 22× versus 182×, serverless grep 7.6–9.9× versus 11.6–13.2×. The
local fork's absolute times are similar or faster; the change comes mostly
from upstream, which is much faster on this host (SSH cat of 100 MiB 7.6 s
versus 31.6 s; serverless 1 GiB cat 26.0 s versus 265.5 s).

Three things differ between the runs and were not separated: the upstream
commit (`d2425e8` versus `91d3500`), the host (bare metal with `tsc` and an
8-core CPU versus a four-vCPU VM on `hpet`) and the local commit (`3b2ec4c`
versus `5eece8d`). The earlier report suspected per-line clock reads as a
major cost on an `hpet` host, which would be consistent with upstream gaining
more from a `tsc` host, but this run did not test that. Treat the ratios as
specific to the host, and use the elapsed seconds, CPU seconds and local
MiB/s above as the reference for this host.

## Reproduction and evidence

```bash
git clone https://github.com/mimecast/dtail.git ../dtail-upstream
# Optional: pin the revision of this report.
git -C ../dtail-upstream checkout d2425e8261d388f1dd2b80ef29cae9308612dffc

# The harness needs clean tracked trees. To measure a fixed commit while the
# main tree is in use (or dirty), benchmark a worktree of it instead.
git worktree add --detach ../dtail-bench HEAD

# Smoke first (builds both trees, untimed correctness checks), then the run,
# on an otherwise idle host with passwordless `sudo -n` for the cache drop.
GOMAXPROCS=4 ./benchmarks/upstream_vs_local_bench.sh smoke \
  --upstream-root ../dtail-upstream --local-root ../dtail-bench
GOMAXPROCS=4 ./benchmarks/upstream_vs_local_bench.sh run \
  --upstream-root ../dtail-upstream --local-root ../dtail-bench \
  --workdir "$(mktemp -d "$HOME/dtail-upstream.XXXXXXXX")" --iterations 5
```

`make benchmark-upstream-smoke` and `make benchmark-upstream` wrap the same
script with default paths (they use the repository itself as the local tree,
so they refuse to run while it has tracked changes). The work directory should
be on a real disk: `/tmp` is a `tmpfs` here, which would make the cache drops
meaningless for the file-read scenarios.

The [compact evidence archive](benchmarks/upstream-2026-11-11/README.md)
contains all 110 raw observations, summary metrics, revisions and
binary/fixture hashes, cache-drop receipts and the run log. It excludes keys,
binaries and payloads.
