# DTail internal documentation

This directory holds **internal, developer-facing** documentation of the
DTail fork. The public user documentation (installation, quick start, query
language, server configuration, usage examples) lives in [`../doc/`](../doc/)
and is linked from the main [README](../README.md).

Layout:

* `dev/` — ongoing developer reference documentation:
  * [Profiling framework](dev/profiling.md) (`make profile-*` targets and the
    `-profile` command flags)
  * [PGO implementation](dev/pgo_implementation.md) and
    [PGO command execution details](dev/pgo_commands_detail.md)
    (`make pgo`, `dtail-tools pgo`)
  * [Performance optimization summary](dev/performance_optimization_summary.md)
    (historical record of an early optimization effort)
  * [Turboboost optimization](dev/turboboost_optimization.md) (design of the
    channel-less read/output path — now the single, default path)
  * [Integration tests refactoring guide](dev/refactoring_guide.md)
* `benchmarks/` — raw evidence bundles (scripts, CSV results, logs) backing
  the dated benchmark reports
* `archive/` — outdated documents kept for historical reference only
* Top level — dated, point-in-time reports such as
  [code-quality-audit-2026-09-06.md](code-quality-audit-2026-09-06.md),
  [performance-plan-2026-09-15.md](performance-plan-2026-09-15.md) and
  [upstream-benchmark-2026-09-23.md](upstream-benchmark-2026-09-23.md)