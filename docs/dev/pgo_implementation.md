# Profile-Guided Optimization (PGO) Implementation for DTail

## Overview

This document describes the Profile-Guided Optimization (PGO) implementation for DTail tools. PGO is a compiler optimization technique that uses runtime profiling data to guide optimization decisions, resulting in better performance for real-world usage patterns.

## Implementation Details

### Architecture

The PGO implementation is integrated into the dtail-tools command as a subcommand:

```bash
dtail-tools pgo [options] [commands...]
```

### Core Components

1. **PGO Module** (`internal/tools/pgo/pgo.go`)
   - Handles the complete PGO workflow
   - Manages profile generation, merging, and PGO builds
   - Provides performance comparison

2. **Profiling Integration**
   - All dtail commands now support the `-profile` flag
   - dserver uses HTTP pprof endpoint for profiling
   - Profiles are generated during realistic workloads

3. **Makefile Integration**
   - `make pgo` - Complete PGO workflow
   - `make pgo-quick` - Quick PGO with smaller datasets
   - `make pgo-generate` - Generate profiles only
   - `make build-pgo` - Build with existing profiles
   - `make install-pgo` - Install PGO-optimized binaries

### Workflow

1. **Build Baseline Binaries**: Standard Go builds without PGO
2. **Generate Profiles**: Run workloads to collect CPU profiles
3. **Merge Profiles**: Combine multiple profile iterations
4. **Build with PGO**: Use profiles to guide optimization
5. **Compare Performance**: Measure improvement

### Profile Generation Details

Each command has specific workloads designed to exercise common code paths:

- **dcat**: Reading large log files
- **dgrep**: Pattern matching with various regex patterns
- **dmap**: MapReduce queries on CSV data
- **dtail**: Following growing log files with filtering
- **dserver**: Handling concurrent client connections

### Special Handling

1. **Profile Verification (fail-loud)**: I/O-bound or idle workloads can capture
   profiles without any CPU samples. Such profiles are worthless for PGO, so
   every captured profile is verified (`verifyProfileNonEmpty` in
   `internal/tools/pgo/pgo.go`): a profile that is missing, 0 bytes, or contains
   zero samples aborts the run with an error. Likewise, `mergeProfiles` fails
   when *all* iteration profiles of a command are empty instead of silently
   writing an empty merged file. This is deliberate: a zero-sample dserver
   capture (an idle server) and a 0-byte dtail capture both slipped through in
   the past and were committed, leaving the two most important server-mode
   binaries with no usable PGO despite documented gains. Failing loudly prevents
   that class of silent empty-profile regression.

2. **dserver Profiling**: Uses HTTP pprof endpoint instead of command-line flags, allowing profile capture during server operation. The capture window is overlapped with sustained, authenticated client load, and the captured profile is checked to be dominated by streaming/read work rather than SSH-handshake crypto (see `pgo_commands_detail.md`).

3. **dtail Workload**: Runs a real follow session against a live dserver with a
   file that keeps growing during the capture (see `pgo_commands_detail.md`)

## Performance Results

### Current measured gains (conservative, 100 MiB serverless run)

On a 100 MB serverless run, PGO on top of DTail's default optimized
read/output path yields workload-dependent and modest improvements:
- **dgrep**: ~7-8%
- **dcat**: ~3%
- **dmap**: within measurement noise

PGO output is byte-identical to non-PGO output.

### Historical point-in-time measurements (superseded)

The figures below were measured at a specific point in time, before the
channel-less read/output path became the single default path, and conflict
with the conservative numbers above; they are kept only as a historical record:
- **dcat**: 3.75-5.40% improvement
- **dgrep**: Up to 19% improvement (varies by pattern hit rate)
- **dmap**: Up to 39% improvement for specific queries

### Overall Performance Progression (historical)
From the original pre-optimization baseline to the build current at the time
of that measurement (the channel-less read/output path, formerly called
"turbo", is now the default and only path):
- **dcat**: 14-21x faster overall
- **dgrep**: 9-15x faster overall
- **dmap**: 9-29% faster overall

## Usage Examples

### Generate PGO-Optimized Binaries
```bash
# Full PGO workflow
make pgo

# Quick PGO with smaller datasets
make pgo-quick

# Generate profiles only
make pgo-generate

# Build with existing profiles
make build-pgo
```

### Using dtail-tools Directly
```bash
# Optimize all commands
dtail-tools pgo

# Optimize specific commands
dtail-tools pgo dcat dgrep

# Verbose mode with custom iterations
dtail-tools pgo -v -iterations 5

# Generate profiles only
dtail-tools pgo -profileonly
```

### Custom PGO Options
```bash
# Custom data size
dtail-tools pgo -datasize 5000000

# Custom profile directory
dtail-tools pgo -profiledir my-profiles

# Custom output directory
dtail-tools pgo -outdir my-pgo-build
```

## Technical Considerations

1. **Profile Quality**: The quality of PGO optimization depends on how representative the profiling workload is of real-world usage.

2. **Binary Size**: PGO-optimized binaries may be slightly larger due to function cloning and inlining decisions.

3. **Build Time**: Building with PGO takes longer than standard builds due to profile processing.

4. **Go Version**: PGO requires Go 1.20 or later.

## Integration with CI/CD

To integrate PGO into your build pipeline:

1. Generate profiles periodically with production-like workloads
2. Keep profiles in an artifact repository when they must be retained; do not
   commit them, because they are workload- and architecture-specific
3. Use `make build-pgo` in your build process
4. Monitor performance metrics to validate improvements

## Profile Files

Profile files are stored in the `pgo-profiles/` directory:
- `dcat.pprof` - DCat CPU profile
- `dgrep.pprof` - DGrep CPU profile
- `dmap.pprof` - DMap CPU profile
- `dtail.pprof` - DTail CPU profile (captured from a live follow session; verified non-empty — a 0-byte dtail.pprof is what used to be committed before verification existed)
- `dserver.pprof` - DServer CPU profile

Every profile is verified to exist, be non-empty, and contain at least one
CPU sample before the PGO build proceeds; profiles are gitignored and
regenerated locally with `make pgo` / `make pgo-generate`.

## Troubleshooting

### Empty or Zero-Sample Profiles
A workload that does not actually exercise the binary under load (e.g. an
idle server or a failed client run) produces a profile with no CPU samples.
This is **not** handled silently: the run fails loudly during profile
verification. Check that the workload drives real work (client load reaches
the server, the dtail session authenticates and streams), fix the workload,
and regenerate the profiles.

### Profile Merge Failures
If profile merging fails, check that:
- All profile files are valid
- Go tools are properly installed
- Sufficient disk space is available

### Performance Not Improving
If PGO doesn't show improvement:
- Ensure profiles represent real workloads
- Check that the profile has sufficient samples
- Verify the correct profile is being used during build

## Future Enhancements

1. **Automated Profile Collection**: Collect profiles from production deployments
2. **Profile Versioning**: Track profile versions with code changes
3. **Multi-Architecture Support**: Generate architecture-specific profiles
4. **Continuous Profiling**: Regular profile updates based on usage patterns
