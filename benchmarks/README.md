# DTail Benchmarks

This directory contains comprehensive benchmarks for the DTail toolset (dcat, dgrep, dmap).

## Overview

The benchmarking framework tests performance across:
- Different file sizes (10MB, 100MB, 1GB)
- Various compression formats (none, gzip, zstd)
- Different query patterns and complexities
- Server mode vs serverless operation

## Prerequisites

Before running benchmarks, ensure all DTail binaries are built:

```bash
cd ..
make build
```

## Running Benchmarks

### Quick Benchmarks (Small Files Only)
```bash
go test -bench=BenchmarkQuick ./benchmarks
```

### All Benchmarks
```bash
go test -bench=. ./benchmarks
```

### Creating Baseline Snapshots
```bash
# Create a baseline before making changes (will prompt for name)
make benchmark-baseline

# Create a quick baseline (small files only, will prompt for name)
make benchmark-baseline-quick

# Create a baseline with a descriptive tag (no prompt)
./dtail-tools benchmark -mode baseline -tag "before-optimization"

# Create a baseline interactively (will prompt if no tag provided)
make benchmark-baseline

# Create a comprehensive baseline (3x iterations)
./dtail-tools benchmark -mode baseline -iterations 3x -tag "v1.0-release"
```

### Comparing Performance
```bash
# Compare with a specific baseline using make
make benchmark-compare BASELINE=benchmarks/baselines/baseline_20240125_143022.txt

# Use the benchmark script for more options
./dtail-tools benchmark -mode compare -baseline benchmarks/baselines/baseline_20240125_143022.txt

# List available baselines
./dtail-tools benchmark -mode list
```

### Specific Tool Benchmarks
```bash
# DCat benchmarks only
go test -bench=BenchmarkDCat ./benchmarks

# DGrep benchmarks only
go test -bench=BenchmarkDGrep ./benchmarks

# DMap benchmarks only
go test -bench=BenchmarkDMap ./benchmarks
```

### With Memory Profiling
```bash
go test -bench=. -benchmem ./benchmarks
```

### Custom Configuration
```bash
# Run with specific file sizes
DTAIL_BENCH_SIZES=small,medium go test -bench=. ./benchmarks

# Keep temporary files for inspection
DTAIL_BENCH_KEEP_FILES=true go test -bench=. ./benchmarks

# Set custom timeout
DTAIL_BENCH_TIMEOUT=30m go test -bench=. ./benchmarks
```

## Benchmark Categories

### DCat Benchmarks
- **Simple**: Sequential file reading
- **Multiple Files**: Reading 10-100 files concurrently
- **Compressed**: Performance with gzip/zstd compression
- **Server Mode**: Client-server performance comparison

### DGrep Benchmarks
- **Simple Pattern**: Basic string matching with varying hit rates
- **Regex Pattern**: Complex regex performance
- **Context Lines**: Impact of --before/--after flags
- **Inverted**: Performance of --invert grep
- **Compressed**: Grep on compressed files

### DMap Benchmarks
- **Simple Aggregation**: Basic count, sum, avg operations
- **Group By Cardinality**: Performance with different group sizes
- **Complex Queries**: WHERE clauses and multiple conditions
- **Time Intervals**: Time-based grouping performance
- **Custom Functions**: Performance of maskdigits, md5sum, etc.

## Output

Benchmark results are saved in multiple formats:
- `benchmark_results/results_TIMESTAMP.json` - Machine-readable JSON
- `benchmark_results/results_TIMESTAMP.csv` - Spreadsheet-compatible CSV
- `benchmark_results/results_TIMESTAMP.md` - Human-readable Markdown report
- `benchmark_results/latest.json` - Most recent results for easy access

## Interpreting Results

Key metrics:
- **MB/sec**: Throughput in megabytes per second
- **lines/sec**: Lines processed per second
- **compression_ratio**: For compressed file benchmarks
- **matched_lines**: For grep benchmarks
- **approx_groups**: For MapReduce group by operations

## Performance Tuning

For accurate benchmarks:
1. Run on isolated hardware
2. Disable CPU frequency scaling
3. Close unnecessary applications
4. Run multiple times and average results

## Continuous Integration

The benchmarks can be integrated into CI/CD pipelines:

```yaml
# Example GitHub Actions workflow
- name: Run Benchmarks
  run: |
    make build
    go test -bench=BenchmarkQuick ./benchmarks
```

## Troubleshooting

### "Command not found" errors
Ensure DTail binaries are built: `make build`

### Disk space issues
Benchmarks create large temporary files. Ensure sufficient disk space (>2GB).

### Timeout errors
Increase timeout: `DTAIL_BENCH_TIMEOUT=60m go test -bench=. ./benchmarks`

## Baseline Management

The benchmarking framework includes tools for creating and comparing performance baselines:

### Creating Baselines
Baselines capture the complete benchmark output including:
- Git commit hash
- Timestamp
- All benchmark results with timing and memory allocation data
- Descriptive names for easy identification

The system will prompt for a meaningful baseline name to ensure proper documentation:

```bash
# Simple baseline (prompts for name)
make benchmark-baseline
> Enter a descriptive name for this baseline: before-cache-optimization

# Quick baseline for rapid testing (prompts for name)
make benchmark-baseline-quick
> Enter a descriptive name for this baseline: initial-performance-check

# Tagged baseline with description (no prompt)
./dtail-tools benchmark -mode baseline -tag "before-refactoring"

# Full baseline with multiple iterations
./dtail-tools benchmark -mode baseline -iterations 3x -memory -tag "release-v2.0"
```

Baseline files are named with the pattern:
`baseline_YYYYMMDD_HHMMSS_descriptive-name.txt`

### Comparing Performance
Compare current performance against a baseline to detect regressions or improvements:

```bash
# Using make
make benchmark-compare BASELINE=benchmarks/baselines/baseline_20240125_143022.txt

# Using benchmark script (provides benchstat analysis if available)
./dtail-tools benchmark -mode compare -baseline benchmarks/baselines/baseline_20240125_143022.txt
```

### Managing Baselines
```bash
# List all baselines
./dtail-tools benchmark -mode list

# View a specific baseline
cat benchmarks/baselines/baseline_20240125_143022.txt

# Clean old baselines (keeps last 10)
./dtail-tools benchmark -mode clean
```

### Best Practices for Baselines
1. Create a baseline before starting optimization work
2. Tag baselines with descriptive names (e.g., "before-cache-impl", "v1.0-release")
3. Use full baselines for release comparisons
4. Commit important baseline files to version control for team reference
5. Run benchmarks on consistent hardware for accurate comparisons

## Contributing

When adding new benchmarks:
1. Follow existing naming conventions
2. Include warmup runs
3. Report relevant metrics
4. Clean up temporary files
5. Document in this README