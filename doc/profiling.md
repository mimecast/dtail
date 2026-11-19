# DTail Profiling Framework

This document describes the profiling framework for dtail commands (dcat, dgrep, dmap) to analyze CPU usage and memory allocations.

## Overview

The profiling framework provides:
- CPU profiling to identify performance bottlenecks
- Memory profiling to track allocations and detect leaks
- Integration with existing benchmarks
- Analysis tools for profile interpretation

## Quick Start

### 1. Build the Tools

```bash
make build  # Builds all tools including dprofile
```

### 2. Run Commands with Profiling

Each command now supports profiling flags:

```bash
# Profile dcat
./dcat -profile -profiledir profiles -plain -cfg none /path/to/file.log

# Profile dgrep with specific profiling types
./dgrep -cpuprofile -memprofile -profiledir profiles -regex "error" /path/to/file.log

# Profile dmap
./dmap -profile -query "select count(*) from data.csv"
```

### 3. Analyze Profiles

Use dtail-tools for quick analysis:

```bash
# List all profiles
./dtail-tools profile -mode list

# Analyze a specific profile
./dtail-tools profile -mode analyze profiles/dcat_cpu_20240101_120000.prof

# Open web browser with flame graph
./dtail-tools profile -mode analyze profiles/dcat_cpu_*.prof -web

# You can also use go tool pprof directly:
go tool pprof profiles/dcat_cpu_20240101_120000.prof
```

## Profiling Options

### Command-line Flags

All dtail commands support these profiling flags:

- `-cpuprofile`: Enable CPU profiling only
- `-memprofile`: Enable memory profiling only
- `-profile`: Enable both CPU and memory profiling
- `-profiledir <dir>`: Directory to store profiles (default: "profiles")

### Profile Types

1. **CPU Profile** (`*_cpu_*.prof`)
   - Samples CPU usage during execution
   - Identifies hot functions and code paths
   - Useful for optimizing computational bottlenecks

2. **Memory Profile** (`*_mem_*.prof`)
   - Captures heap allocations at end of execution
   - Shows memory usage by function
   - Helps identify memory leaks

3. **Allocation Profile** (`*_alloc_*.prof`)
   - Tracks all allocations during execution
   - More detailed than memory profile
   - Useful for reducing allocation pressure

## Using with Benchmarks

### Automated Profiling

Run profiling using dtail-tools:

```bash
# Quick profiling with small datasets
./dtail-tools profile -mode quick

# Full profiling suite
./dtail-tools profile -mode full

# Profile dmap specifically (with MapReduce format)
./dtail-tools profile -mode dmap
```

This tool:
- Generates test data of various sizes
- Profiles dcat, dgrep, and dmap with different workloads  
- Stores profiles in the `profiles` directory
- Provides immediate analysis of results

### Using Make Targets

```bash
# Quick profiling with immediate results
make profile-quick

# Full profiling suite
make profile-all

# Profile dmap specifically
make profile-dmap

# List available profiles
make profile-list

# Analyze a specific profile
make profile-analyze PROFILE=profiles/dcat_cpu_*.prof

# Open web interface for profile
make profile-web PROFILE=profiles/dcat_cpu_*.prof
```

### Benchmark Integration

Run profiling-enabled benchmarks:

```bash
cd benchmarks
go test -bench="WithProfiling" -benchtime=1x
```

### Custom Profile Runner

Use the profile runner in your benchmarks:

```go
import "github.com/mimecast/dtail/benchmarks"

func BenchmarkMyFeature(b *testing.B) {
    benchmarks.ProfileBenchmark(b, "MyFeature", "dcat", 
        "--plain", "--cfg", "none", "testfile.log")
}
```

## Profile Analysis

### Using go tool pprof

For interactive analysis:

```bash
# Interactive mode
go tool pprof profiles/dcat_cpu_*.prof

# Common pprof commands:
# top       - Show top functions
# list func - Show source code for function
# web       - Generate SVG graph
# peek func - Show callers/callees of function
```

Generate visualizations:

```bash
# Flame graph (requires graphviz)
go tool pprof -http=:8080 profiles/dcat_cpu_*.prof

# Generate SVG
go tool pprof -svg profiles/dgrep_mem_*.prof > profile.svg

# Generate text report
go tool pprof -text profiles/dmap_alloc_*.prof > report.txt
```

### Using dtail-tools profile

The dtail-tools profile command provides quick summaries:

```bash
# List all profiles
./dtail-tools profile -mode list

# Analyze specific profile
./dtail-tools profile -mode analyze profiles/dcat_cpu_20240101_120000.prof

# Get help
./dtail-tools profile -h
```

## Optimization Workflow

1. **Baseline Performance**
   ```bash
   # Run benchmarks without profiling
   cd benchmarks
   go test -bench="BenchmarkDCat" -benchtime=10s
   ```

2. **Profile Execution**
   ```bash
   # Run with profiling
   ./dcat -profile -profiledir profiles large_file.log
   ```

3. **Identify Bottlenecks**
   ```bash
   # Analyze CPU profile
   ./dprofile -profile profiles/dcat_cpu_*.prof -top 10
   
   # Check memory allocations
   go tool pprof -alloc_space profiles/dcat_alloc_*.prof
   ```

4. **Optimize Code**
   - Focus on functions with high Flat% (direct CPU usage)
   - Reduce allocations in hot paths
   - Consider buffering and pooling

5. **Verify Improvements**
   ```bash
   # Re-run benchmarks after optimization
   go test -bench="BenchmarkDCat" -benchtime=10s
   ```

## Common Performance Issues

### CPU Bottlenecks

Look for:
- Regex compilation in loops
- Excessive string operations
- Inefficient algorithms (O(n²) or worse)
- Unnecessary type conversions

Example optimization:
```go
// Before: Regex compiled every time
for _, line := range lines {
    if regexp.MustCompile(pattern).MatchString(line) {
        // ...
    }
}

// After: Compile once
re := regexp.MustCompile(pattern)
for _, line := range lines {
    if re.MatchString(line) {
        // ...
    }
}
```

### Memory Issues

Common patterns:
- String concatenation in loops
- Large temporary slices
- Unclosed resources
- Excessive goroutines

Example optimization:
```go
// Before: Many allocations
result := ""
for _, s := range strings {
    result += s + "\n"
}

// After: Single allocation
var buf strings.Builder
buf.Grow(estimatedSize)
for _, s := range strings {
    buf.WriteString(s)
    buf.WriteByte('\n')
}
result := buf.String()
```

## Tips and Best Practices

1. **Profile Real Workloads**
   - Use production-like data sizes
   - Test with actual file formats
   - Include network operations if relevant

2. **Compare Profiles**
   ```bash
   # Compare before/after optimization
   go tool pprof -diff_base=before.prof after.prof
   ```

3. **Focus on Hot Paths**
   - Optimize functions with >5% CPU usage first
   - Small improvements in hot paths have big impact

4. **Memory Profiling**
   - Use `-alloc_space` for total allocations
   - Use `-inuse_space` for current heap usage
   - Check for growing heap over time

5. **Benchmark Regularly**
   - Add profiling to CI/CD pipeline
   - Track performance over releases
   - Set performance regression alerts

## Troubleshooting

### No profiles generated
- Check write permissions for profile directory
- Ensure command completes successfully
- Verify profiling flags are correct

### Empty or small profiles
- Run command with larger workload
- Increase execution time
- Check if command exits too quickly

### Analysis tools fail
- Ensure profile format is valid
- Check Go version compatibility
- Verify graphviz is installed for visualizations

## Advanced Usage

### Custom Profiling Points

Add profiling snapshots in code:

```go
import "github.com/mimecast/dtail/internal/profiling"

func processLargeFile() {
    profiler := profiling.GetProfiler() // Assumes global profiler
    
    // Take memory snapshot before processing
    profiler.Snapshot("before_processing")
    
    // ... process file ...
    
    // Take snapshot after
    profiler.Snapshot("after_processing")
}
```

### Continuous Profiling

For long-running operations:

```go
// Start periodic metrics logging
ticker := time.NewTicker(30 * time.Second)
go func() {
    for range ticker.C {
        profiler.LogMetrics("periodic")
    }
}()
defer ticker.Stop()
```

## Contributing

When adding new features:
1. Include benchmark tests
2. Run profiling before submitting PR
3. Document any performance implications
4. Add profiling examples for new commands

## References

- [Go Profiling Documentation](https://go.dev/blog/pprof)
- [pprof Tool Guide](https://github.com/google/pprof)
- [Go Performance Tips](https://go.dev/wiki/Performance)