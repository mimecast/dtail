# DTail Performance Benchmark Results

This document summarizes the performance comparison between the **Upstream** reference implementation and the **Local** optimized fork.

Last run: 2026-09-21, local fork at `df52a1e`, upstream (`github.com/mimecast/dtail`) at `91d3500`. It was the closure run of `docs/performance-plan-2026-09-15.md`, which also records the per-task measurements.

## Test Methodology
- **Tool**: `benchmarks/upstream_vs_local_bench.sh run --iterations 5`, after its `smoke` mode had checked that both implementations produce matching output.
- **Cold Start**: The system page cache was dropped using `drop_caches` before every single observation to eliminate memory-caching bias.
- **Interleaved Runs**: Each iteration ran both implementations, alternating which one went first (Upstream then Local, then Local then Upstream), to ensure fairness.
- **Modes**:
    - **Serverless (`sl_`)**: Direct disk access.
    - **Server (`sv_`)**: Communication via `dserver` over SSH.
- **Data Sets**: 100 MiB and 1 GiB deterministic log files, plus a 10 MiB burst for the follow scenario.
- **Host**: Rocky Linux 9 bhyve guest, 4 vCPUs, Linux 5.14, go1.26.6. Its clocksource is `hpet`, which makes every clock read a real syscall (see the plan document).
- **Samples**: 5 per scenario and implementation, 0 failures in every scenario.

## Summary Results (Upstream versus Local)

From the first run (upstream `91d3500` against the local fork).

| Scenario | Samples | Upstream Median | Local Median | Speedup | Upstream MiB/s | Local MiB/s |
|---|---:|---:|---:|---:|---:|---:|
| **sl_dcat_large** (1 GiB) | 5 | 268.26 s | 1.46 s | **183.6x** | 3.82 | 700.94 |
| **sl_dcat_medium** (100 MiB) | 5 | 26.37 s | 0.20 s | **132.0x** | 3.79 | 500.66 |
| **sl_dgrep_high** (100 MiB) | 5 | 2.81 s | 0.16 s | **17.6x** | 35.63 | 628.54 |
| **sl_dgrep_low** (100 MiB) | 5 | 2.62 s | 0.22 s | **11.8x** | 38.21 | 451.82 |
| **sl_dmap_aggregate** (100 MiB) | 5 | 41.19 s | 0.85 s | **48.6x** | 2.43 | 117.94 |
| **sv_dcat_medium** (100 MiB) | 5 | 31.54 s | 0.64 s | **49.1x** | 3.17 | 155.64 |
| **sv_dgrep_high** (100 MiB) | 5 | 3.60 s | 0.35 s | **10.3x** | 27.79 | 286.12 |
| **sv_dgrep_low** (100 MiB) | 5 | 2.90 s | 0.31 s | **9.5x** | 34.46 | 327.56 |
| **sv_dmap_aggregate** (100 MiB) | 5 | 37.82 s | 0.94 s | **40.2x** | 2.64 | 106.41 |
| **sv_dmap_count** (100 MiB) | 5 | 37.60 s | 0.76 s | **49.3x** | 2.66 | 131.11 |
| **sv_dtail_follow** (10 MiB) | 5 | 3.96 s | 0.25 s | **15.7x** | 2.52 | 39.62 |

## Effect of the 2026-09-15 Performance Plan

The same script was run a second time with the pre-plan fork commit `c472f83` in place of upstream. This isolates what the performance plan changed. Ranges are min-max over the 5 samples. The "Current" column comes from this second run, so its medians differ slightly from the Local column above for the same build (for example 0.18 s versus 0.20 s for sl_dcat_medium), which gives an idea of the run-to-run noise.

| Scenario | Pre-plan (`c472f83`) median | Current median | Speedup | Pre-plan range | Current range |
|---|---:|---:|---:|---:|---:|
| **sl_dcat_large** (1 GiB) | 1.87 s | 1.47 s | 1.28x | 1.86-1.90 s | 1.44-1.50 s |
| **sl_dcat_medium** (100 MiB) | 0.24 s | 0.18 s | 1.31x | 0.23-0.40 s | 0.18-0.27 s |
| **sl_dgrep_high** (100 MiB) | 0.17 s | 0.16 s | 1.05x | 0.16-0.24 s | 0.15-0.19 s |
| **sl_dgrep_low** (100 MiB) | 0.19 s | 0.21 s | 0.93x | 0.19-0.24 s | 0.20-0.25 s |
| **sl_dmap_aggregate** (100 MiB) | 1.87 s | 0.85 s | 2.20x | 1.83-1.89 s | 0.84-0.90 s |
| **sv_dcat_medium** (100 MiB) | 8.67 s | 0.64 s | 13.56x | 8.60-8.69 s | 0.59-0.70 s |
| **sv_dgrep_high** (100 MiB) | 0.99 s | 0.35 s | 2.83x | 0.95-1.01 s | 0.32-0.38 s |
| **sv_dgrep_low** (100 MiB) | 0.30 s | 0.31 s | 0.98x | 0.27-0.33 s | 0.27-0.33 s |
| **sv_dmap_aggregate** (100 MiB) | 1.99 s | 0.94 s | 2.11x | 1.96-2.05 s | 0.94-0.96 s |
| **sv_dmap_count** (100 MiB) | 1.76 s | 0.76 s | 2.31x | 1.66-1.82 s | 0.75-0.80 s |
| **sv_dtail_follow** (10 MiB) | 1.27 s | 0.28 s | 4.58x | 1.25-1.29 s | 0.25-0.29 s |

The two dgrep low-match scenarios show no measurable change: their pre-plan and current ranges overlap. Serverless dgrep high-match and serverless dcat 100 MiB also overlap, although their medians improved.

## Key Findings

### 1. Massive I/O Throughput Increase
In serverless `dcat` tests, the local version reads at up to **701 MiB/s**, compared to the upstream version's **~3.8 MiB/s**, a ~184x increase on the 1 GiB file.

### 2. Server Mode Caught Up
Server-mode `dcat` was the slowest local scenario before the performance plan (8.67 s for 100 MiB, bound by per-line clock reads and copies). It now takes 0.64 s, 49x faster than upstream and 13.6x faster than the pre-plan fork.

### 3. Efficient MapReduce Processing
The MapReduce (`dmap`) scenarios are **40x to 49x faster** than upstream in both modes, and 2.1x to 2.3x faster than before the performance plan, which removed the per-line allocations and the shared per-line lock of the aggregation path.

## Conclusion
The local fork is 9.5x to 184x faster than upstream across all measured scenarios, reading, searching, aggregating and following, in both serverless and server mode.

## Stability and Repeatability
In the MapReduce scenarios of the upstream comparison run, the local version's 5 cold-cache samples stayed within 4-9% of each other (0.84-0.91 s serverless and 0.94-0.97 s server-mode dmap aggregate, 0.74-0.81 s dmap count), while upstream varied by 13-18% (38.5-44.0 s, 35.8-42.3 s and 36.6-41.4 s). The short local scenarios vary more in relative terms: for example serverless dgrep high-match ranged 0.16-0.21 s and the follow burst 0.21-0.29 s, where a few tens of milliseconds of noise are a large share.
