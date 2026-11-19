# Evidence for the 2026-09-23 upstream comparison

See the [report](../../upstream-benchmark-2026-09-23.md) for method and caveats.

- [results.csv](results.csv): all 110 timed observations; zero failures.
- [summary.csv](summary.csv): per-scenario medians, throughput, CPU and RSS.
- [metadata.txt](metadata.txt): exact revisions, build environment and hashes.
- [cache-drops.log](cache-drops.log): 114 successful guest cache-drop receipts.
- [run.log](run.log): build, smoke-check and scenario completion log.

Archived text has LF line endings and trailing whitespace removed; metric
values are unchanged. `local_change_percent` in the generated summary means
`(upstream/local - 1) * 100`, not percent elapsed-time reduction. Prefer the
`local_speedup_x` column.

Post-run verification: each of the ten full follow outputs, after removing
its `PROBE-<build>-<round>-yes ` and `BENCHEND-<build>-<round>-yes` marker lines,
matched `data/follow_10mib.log` with `cmp`. Payloads are not archived here.
The original scratch directory was `/tmp/dtail-upstream-20260923.ZBR3ebEX`;
its absolute paths in metadata/logs are provenance, not repository links.
