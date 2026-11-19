# Evidence for the 2026-11-11 upstream comparison

See the [report](../../upstream-benchmark-2026-11-11.md) for method and caveats.

- [results.csv](results.csv): all 110 timed observations; zero failures.
- [summary.csv](summary.csv): per-scenario medians, throughput, CPU and RSS.
- [metadata.txt](metadata.txt): exact revisions, build environment and hashes.
- [cache-drops.log](cache-drops.log): 114 successful cache-drop receipts.
- [run.log](run.log): build, smoke-check and scenario completion log.

Archived text has LF line endings and trailing whitespace removed; metric
values are unchanged. `local_change_percent` in the generated summary means
`(upstream/local - 1) * 100`, not percent elapsed-time reduction. Prefer the
`local_speedup_x` column. Scenario prefixes: `sl_` is serverless, `sv_` is
loopback SSH server.

The original work directory was outside the repository; its absolute path is
replaced by `<workdir>` in metadata and logs. The local `root` in
`metadata.txt` is a detached `git worktree` of the local repository at the
recorded commit, used so the measured source stays pinned to that commit
(the harness refuses a checkout with tracked changes).

Post-run verification: each of the twelve full follow outputs (five timed
observations and one warm-up per implementation), after removing its
`PROBE-<build>-<round>-yes ` and `BENCHEND-<build>-<round>-yes` marker lines,
matched `data/follow_10mib.log` with `cmp`. Payloads are not archived here.
