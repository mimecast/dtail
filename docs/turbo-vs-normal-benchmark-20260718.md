# Turbo vs Normal (Non-Turbo) Benchmark Report — 2026-07-18

**Question:** After the recent server/turbo-path work (in particular task `ws0`,
which gave `TurboAggregate` an input-exhausted `FinishInput` signal), is turbo
mode still faster than the normal (channel-based) path — and does the
server-mode `dmap` deadlock the previous report flagged still exist?

**Answer: Yes — turbo remains faster in every scenario where it applies, and the
result is reproducible across two independent full runs. The server-mode `dmap`
turbo deadlock documented in the 2026-07-10 report is FIXED: turbo `dmap` in
server mode now completes and is ~1.6×–2.6× faster than non-turbo. Server-mode
`dcat`, previously transport-bound and neutral, is now ~6–8× faster on the turbo
path.**

## Methodology

- Binaries rebuilt from clean at HEAD `1a079d9` (`make clean && make build`).
- Turbo is default-on; non-turbo runs set `DTAIL_TURBOBOOST_DISABLE=yes` on the
  process that runs the server handlers (the client itself in serverless mode,
  `dserver` in server mode).
- Serverless scenarios: client binaries run directly against local files.
- Server scenarios: two `dserver` instances on localhost (one turbo, one
  non-turbo, `--cfg none`, real SSH auth via a dedicated RSA key + `CacheDir`
  authorized_keys) — *not* `DTAIL_INTEGRATION_TEST_RUN_MODE`, because that mode
  force-disables turbo (`internal/config/initializer.go`) and would compare
  non-turbo against non-turbo.
- Turbo/non-turbo iterations alternate A/B to spread thermal/load drift (dcat,
  dgrep, dmap scenarios); the dtail follow scenario runs all turbo iterations
  first, then all non-turbo. 1 warmup + 5–7 measured iterations per
  scenario/mode; medians reported.
- Wall-clock timing, client stdout to `/dev/null` (except dtail follow).
- Datasets: `benchmarks/testdata/medium.log` (100 MB, 1.22 M lines),
  `large.log` (1 GB), and a generated 100 MB `MAPREDUCE:STATS` file (800 k
  lines, 50 hostnames). dgrep patterns: `"user999 "` (low hit rate, ~0.07 %) and
  `"ERROR"` (high hit rate, ~19 %).
- dtail follow: client follows an empty file over SSH; after the stream is
  established (probe line round-trip), a 10 MB / 100 k-line burst plus end
  marker is appended; time from append to marker arrival at the client is
  measured, and delivered-line completeness is counted.
- The benchmark was run twice end to end (run 1 and run 2) to check
  reproducibility. Raw per-iteration data is committed alongside this report:
  `turbo-vs-normal-benchmark-20260718-run1.csv` and `...-run2.csv`. Harness:
  `benchmarks/turbo_vs_normal_bench.sh`.
- Machine: 11th Gen Intel i7-1185G7 @ 3.00 GHz (8 threads), Fedora Linux 44,
  Go 1.26.4. Caveat: laptop; absolute numbers are noisy (thermal/background
  load), so the **turbo/non-turbo ratios are the meaningful signal**, not the
  absolute seconds. This is visible in the server-mode rows, where run 2's
  non-turbo times are lower than run 1's (lighter machine load), which
  compresses those ratios without any change to the turbo path.
- Non-turbo serverless runs go through an `env` wrapper that turbo runs do not
  (~1 ms extra process spawn). Negligible at these magnitudes, and it biases
  against turbo, not for it.

## Results (median wall time, seconds)

### Serverless mode

| Scenario                     | Turbo (r1 / r2) | Non-Turbo (r1 / r2) | Speedup (r1 / r2) | Verdict |
|------------------------------|-----------------|---------------------|-------------------|---------|
| dcat 100 MB                  | 0.094 / 0.094   | 1.310 / 1.322       | 13.9× / 14.1×     | turbo much faster |
| dcat 1 GB                    | 0.991 / 1.034   | 17.776 / 17.912     | 17.9× / 17.3×     | turbo much faster |
| dgrep 100 MB, low hit rate   | 0.106 / 0.111   | 2.104 / 2.289       | 19.8× / 20.6×     | turbo much faster |
| dgrep 100 MB, high hit rate  | 0.098 / 0.104   | 2.184 / 2.342       | 22.3× / 22.5×     | turbo much faster |
| dmap agg 100 MB (control)    | 3.072 / 2.999   | 3.279 / 3.068       | 1.07× / 1.02×     | expected: turbo N/A serverless |

Serverless ratios are extremely stable between the two runs (dcat ~14–18×,
dgrep ~20–22×). Serverless `dmap` shows parity because turbo is a server-side
optimization; client-side aggregation does not use it (as documented).

### Server mode (localhost SSH)

| Scenario                     | Turbo (r1 / r2) | Non-Turbo (r1 / r2) | Speedup (r1 / r2) | Verdict |
|------------------------------|-----------------|---------------------|-------------------|---------|
| dcat 100 MB                  | 1.962 / 1.253   | 15.263 / 8.274      | 7.8× / 6.6×       | turbo much faster |
| dgrep 100 MB, low hit rate   | 0.783 / 0.725   | 5.524 / 2.176       | 7.1× / 3.0×       | turbo faster |
| dgrep 100 MB, high hit rate  | 0.962 / 0.763   | 6.199 / 2.893       | 6.4× / 3.8×       | turbo faster |
| dmap count, 100 MB stats     | 2.442 / 1.491   | 6.113 / 3.841       | 2.5× / 2.6×       | **turbo faster — now completes (was deadlock)** |
| dmap group-by-agg, 100 MB    | 2.729 / 1.692   | 5.318 / 2.667       | 1.9× / 1.6×       | **turbo faster — now completes (was deadlock)** |
| dtail follow 10 MB burst     | 0.240 / 0.115, **100 % delivered** | timeout, ~39–47 % delivered* | n/a | turbo strictly better |

\* Non-turbo tail drops lines when the consumer lags (documented behavior,
`internal/io/line/line.go`). Across both runs the non-turbo follow delivered
only ~39 000–47 000 of 100 000 lines and generally never received the end
marker, timing out at >90 s. Turbo delivered all 100 000 lines in ~0.1–0.24 s in
every iteration.

## Per-tool verdicts

- **dcat**: turbo a big win serverless (~14–18×). In server mode it is now
  ~6–8× faster (previously transport-bound and neutral) — the turbo network
  writer path (64 KB buffering, `bytesWritten` accounting) pays off over SSH.
- **dgrep**: turbo a big win everywhere (serverless ~20–22×; server ~3–7×,
  larger under heavier load).
- **dtail**: turbo is strictly better — complete delivery in ~0.1 s vs lossy
  (~40 %) delivery that times out on the normal path.
- **dmap (serverless)**: turbo not applicable; parity confirmed.
- **dmap (server mode)**: **now works.** Turbo aggregation completes and is
  ~1.6×–2.6× faster than non-turbo. See the "what changed" note below.

## What changed since the 2026-07-10 report

The previous report (`benchmarks/turbo_vs_normal_report_20260710.md`) found
turbo dramatically faster everywhere it applied **but** uncovered a functional
deadlock: server-mode `dmap` with turbo enabled (the default) never terminated
the client session, because nothing signaled "input exhausted" to
`TurboAggregate` — the map command stayed active, so the shutdown coordinator's
`activeCommands == 0` finalization never ran.

Task `ws0` fixed this by giving `TurboAggregate` a `FinishInput()` that performs
the final flush + serialize + `done.Shutdown()` once input is exhausted,
analogous to the regular aggregate's channel-close path. This benchmark confirms
the fix end to end: server-mode turbo `dmap` (both `count group by` and the
multi-aggregation query) completes cleanly in every iteration of both runs and
is faster than the non-turbo path. No hangs, no `timeout`/`rc124` rows.

## Reproducing

```bash
make clean && make build
bash benchmarks/turbo_vs_normal_bench.sh /tmp/dtail_turbo_bench
# medians:
awk -F, 'NR>1 && $4!="" {k=$1","$2; v[k]=v[k]" "$4}
  END{for(x in v){n=split(v[x],a," ");asort(a);
  m=(n%2)?a[(n+1)/2]:(a[n/2]+a[n/2+1])/2;printf "%s,%.3f\n",x,m}}' \
  /tmp/dtail_turbo_bench/results.csv | sort
```
